# Media route settings

Last updated 2026-11-28.

The model behind each media tier (posters, video clips, presenters, voice) is a platform setting that I keep in ProviderSetting `platform / media / routes`, one key per tier (`media.route.poster_premium` and so on). marketflow-ai reads the row and runs each route's default for a key that is not set. Its `docs/media-routes.md` lists every key, its default and how the values are checked there.

Code: `internal/modules/admin/media_routes.go`, `internal/platform/modelcatalog/autopick.go`, and the Sync models handler in `internal/modules/admin/sync_models.go`.

## Endpoints (platform owner only)

| Endpoint | What it does |
|---|---|
| `GET /api/v1/admin/providers/media-routes?card=fal` | Each tier: label, kind, fal categories, default, saved value, what runs now and the value the last change replaced. `card` is `fal` or `elevenlabs`; leave it out for every tier. |
| `PATCH /api/v1/admin/providers/media-routes` | Body `{"settings": {"media.route.video_standard": "..."}}`. Only the keys sent change. An empty value resets the tier to its default (deletes the key). A changed value keeps the one it replaced under `<key>.previous`, which the console offers as Revert. |
| `GET /api/v1/admin/providers/fal-pricing?ids=a,b` | fal's unit price for up to 50 endpoints, read with the platform fal key and cached a day. |

`POST /api/v1/admin/providers/configure` replaces every key of a provider, so it refuses `media/routes`. That keeps one tier's save from wiping the others.

## Checks on save

- A fal id must look like an endpoint id, be in fal's model list and have a category that fits the tier. The list comes from the last fal Sync models (kept six hours) or is read on the save within 20 seconds. When it cannot be read, I check the format only and marketflow-ai checks again when it runs.
- An ElevenLabs id must look like `eleven_...`. The Scribe flag is `true` or `false`.
- Unknown keys and the `.previous` keys are refused; the server keeps those.

Every change writes an audit row (`admin.media_routes_updated`).

## Sync models upgrades

Sync models on the fal.ai or ElevenLabs card compares each of the card's tiers with the synced list. When the list has a newer version of the same family and variant (the id with its numbers replaced matches, the version is higher, the category is the same and the id is not a preview, alpha, beta or similar build), the tier switches to it. The replaced id goes to `<key>.previous` for revert, an audit row is written (`admin.media_routes_auto_upgraded`) and the platform admins get the change on Slack (auth-api's platform alert channel) and by email, through the same alert path as the daily model watch. The reply lists the switches in `auto_switched`.

A different family or variant (Seedream v5 Lite to Seedream v5.5, Hailuo standard to Hailuo pro) is never switched to on its own. It shows as a recommendation for the owner to compare in the picker.

The fal list is now read with each model's category and release date (`DescribeFal`), and deprecated models are left out of it.

The swagger file (`internal/httpapi/docs.go`) does not have the three new routes yet; it needs a regeneration.
