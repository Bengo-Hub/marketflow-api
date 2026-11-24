//go:generate go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/modifier,sql/upsert,sql/versioned-migration,sql/execquery ./schema
package ent
