// Package main is the entrypoint for the marketflow worker binary.
//
// Responsibilities:
//   - Consume NATS JetStream subjects:
//       nurture.step.due    → dispatch via notifications-api, advance enrollment
//       embedding.requested → KB embedding pipeline (Ollama)
//       chat.completed      → audit sink
//       lead.created        → auto-enrol the lead in the tenant's default nurture sequence
//   - Run a periodic scheduler that publishes `nurture.step.due` for enrollments
//     whose next_step_at has elapsed.
//   - Expose /healthz for k8s liveness probes.
//
// Ships in the same Docker image as marketflow-api (cmd/api + cmd/worker split).
// The container entrypoint starts the worker in the background before exec'ing
// the API.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	sharedcache "github.com/Bengo-Hub/cache"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// workerSignalSubjects are the marketflow-internal work-signal subjects the worker
// stream must bind IN ADDITION to the outbox-published marketflow.> events. No other
// stream binds these prefixes, so extending this stream is conflict-free.
var workerSignalSubjects = []string{
	"marketflow.>",
	"nurture.>",
	"crm.>",
	"embedding.>",
	"chat.>",
	"lead.>",
}

// ensureWorkerStream creates the worker stream when missing, or reconciles an existing
// stream's subject set by MERGING the required bindings into it (never rewriting the
// rest of the config — prod streams carry retention/replica settings that must survive).
func ensureWorkerStream(ctx context.Context, js jetstream.JetStream, name string) error {
	s, err := js.Stream(ctx, name)
	if err != nil {
		_, cErr := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     name,
			Subjects: workerSignalSubjects,
			Storage:  jetstream.FileStorage,
			MaxAge:   72 * time.Hour,
		})
		return cErr
	}
	cfg := s.CachedInfo().Config
	have := make(map[string]bool, len(cfg.Subjects))
	for _, subj := range cfg.Subjects {
		have[subj] = true
	}
	missing := false
	for _, subj := range workerSignalSubjects {
		if !have[subj] {
			cfg.Subjects = append(cfg.Subjects, subj)
			missing = true
		}
	}
	if !missing {
		return nil
	}
	_, err = js.UpdateStream(ctx, cfg)
	return err
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	cfg, err := worker.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Lease client for once-per-period scheduled work (this worker runs in every pod).
	rdb, rerr := sharedcache.NewRedis(ctx, sharedcache.RedisConfig{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB, TLS: cfg.Redis.EnableTLS,
	})
	if rerr != nil {
		logger.Warn("worker: redis not reachable at startup; scheduled jobs skip until it is", zap.Error(rerr))
	}
	sharedcache.SetLeaseClient(rdb)

	// ── DB (optional — enables auto-enrol + scheduler) ────────────────────
	var entClient *ent.Client
	if cfg.Database.URL != "" {
		db, err := sql.Open("pgx", cfg.Database.URL)
		if err != nil {
			log.Fatalf("open postgres: %v", err)
		}
		defer db.Close()
		if err := db.PingContext(ctx); err != nil {
			logger.Warn("postgres ping failed — scheduler + auto-enrol disabled", zap.Error(err))
		} else {
			drv := entsql.OpenDB(dialect.Postgres, db)
			entClient = ent.NewClient(ent.Driver(drv))
			defer entClient.Close()
			logger.Info("ent client ready")
		}
	} else {
		logger.Warn("POSTGRES_URL unset — scheduler + auto-enrol disabled")
	}

	// ── NATS ──────────────────────────────────────────────────────────────
	nc, err := nats.Connect(cfg.NATS.URL, nats.Name("marketflow-worker"))
	if err != nil {
		log.Fatalf("nats: %v", err)
	}
	defer nc.Drain()

	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("jetstream: %v", err)
	}

	// Ensure the worker's own stream captures the marketflow-internal work-signal
	// subjects (nurture.>/crm.>/embedding.>/chat.>/lead.>) in addition to the
	// outbox-published marketflow.> events — no other stream binds them, so without
	// this every JS publish of a work signal fails and durable consumers have
	// nothing to bind to. Cross-service subjects (ordering./pos./treasury.) are NOT
	// bound here; the registry binds those consumers to their owning streams.
	if err := ensureWorkerStream(ctx, js, cfg.NATS.Stream); err != nil {
		log.Fatalf("ensure %s stream: %v", cfg.NATS.Stream, err)
	}

	var rawDB *sql.DB
	if entClient != nil {
		// Re-open a raw *sql.DB for the pgvector upsert path. Uses the same
		// POSTGRES_URL; connection pool is independent of Ent's.
		if raw, err := sql.Open("pgx", cfg.Database.URL); err == nil {
			if err := raw.PingContext(ctx); err == nil {
				rawDB = raw
				defer raw.Close()
			}
		}
	}
	// Provider-secret key: DB-first (platform ServiceConfig), then the same env key the API uses.
	// This worker runs in the API pod, so MF_SECURITY_ENCRYPTION_KEY is the API's own value.
	keys := encryption.NewKeyProvider(entClient, os.Getenv("MF_SECURITY_ENCRYPTION_KEY"))
	reg := worker.NewRegistry(cfg, entClient, rawDB, keys, logger)
	if err := reg.RegisterAll(ctx, js); err != nil {
		log.Fatalf("register consumers: %v", err)
	}
	logger.Info("consumers registered", zap.Int("count", reg.Count()))

	// ── Scheduler ─────────────────────────────────────────────────────────
	if entClient != nil {
		sched := worker.NewScheduler(entClient, nc, cfg.Scheduler, logger)
		go sched.Run(ctx)
		reeng := worker.NewReEngagement(entClient,
			time.Duration(cfg.Scheduler.ReengagementIntervalHours)*time.Hour,
			time.Duration(cfg.Scheduler.ReengagementDormantDays)*24*time.Hour,
			logger,
		)
		go reeng.Run(ctx)

		// Chat transcript retention (30 days after last activity by default).
		go worker.NewChatRetention(entClient, cfg.Scheduler.ChatRetentionDays, logger).Run(ctx)

		// Ad-insights polling cron (Meta campaigns only today). Resolver
		// pulls the tenant's Meta page token from ProviderSetting at query
		// time; falls back to the Tenant.meta_access_token legacy field
		// when the ProviderSetting row is missing.
		tokenResolver := worker.NewMetaTokenResolver(entClient, keys, logger)
		insights := worker.NewAdInsights(entClient, 15*time.Minute, tokenResolver.Resolve, logger)
		go insights.Run(ctx)

		// CRM background agents: ProactiveFollowUpAgent (hourly) + DealStaleDetector (daily).
		cronAgents := worker.NewCronAgentRunner(worker.CronAgentRunnerConfig{
			DB:         entClient,
			APIBaseURL: cfg.API.MarketFlowAPIURL,
			ServiceKey: cfg.API.InternalServiceKey,
			Logger:     logger,
		})
		go cronAgents.Run(ctx)

		// SLA breach detection: every 5 min, queries ticketing-api and marks breached tickets.
		slaCron := worker.NewSLABreachCron(
			cfg.API.TicketingAPIURL,
			cfg.Notifications.URL,
			cfg.API.InternalServiceKey,
			logger,
			&http.Client{Timeout: 15 * time.Second},
		)
		go slaCron.Run(ctx)

		// MeetingPrepAgent: every 15 min, sends prep briefs 25–35 min before meetings.
		meetingPrep := worker.NewMeetingPrepAgent(
			entClient,
			cfg.API.MarketFlowAIURL,
			cfg.API.MarketFlowAPIURL,
			cfg.API.InternalServiceKey,
			cfg.Notifications.URL,
			logger.Named("agent.meeting-prep"),
			&http.Client{Timeout: 30 * time.Second},
		)
		go meetingPrep.Run(ctx)
	}

	// ── Health endpoint ───────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"service":    "marketflow-worker",
			"consumers":  reg.Count(),
			"scheduler":  entClient != nil,
			"started_at": time.Now().UTC(),
		})
	})
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTP.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("worker health server listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server", zap.Error(err))
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutdown signal received")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}
