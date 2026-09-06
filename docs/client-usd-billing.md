# Client USD billing rollout

The deployment updates CLIProxyAPI to upstream `5208aec7` (v7.2.151) and CPA Manager Plus to upstream `7c4cbead` (v1.12.8), preserving local client usage, API key quotas, workspace names, and request handling changes.

## Billing contract

- Client limits are `cost-limits: {12h: <USD>, 7d: <USD>}`. Omitted or zero limits are unlimited.
- Existing limits convert at **1,000,000 old quota units = $0.40**, per the user's GPT-5.6 Sol choice. This matches the old input/cache ratio; it does not equate the old output weight with Sol's current output price.
- Server-side integer nanodollars accumulate immutable per-request `cost_usd` snapshots. JSON/YAML expose dollar decimals, with at most 9 decimal places.
- Unknown model prices use GPT-6 Astra ($10 input, $1 cache read, $12.50 cache write, $50 output per million tokens), including applicable context and service-tier multipliers.
- New requests use the canonical upstream token breakdown: input includes cache buckets; output includes reasoning exactly once. Upstream response service tier takes precedence when available.
- The client-usage page consumes authoritative backend costs, shows input/output separated by `/`, and retains cache hit rate without a separate cache-token column.
- Limits are checked before a request. Already admitted or concurrent requests can finish above the limit; subsequent requests are rejected.
- OpenAI price source: https://developers.openai.com/api/docs/pricing (verified 2026-09-05). Other known-model base rates retain the existing client-usage catalog.

## Data migration

The offline `migrate-client-billing` tool must run while CPA is stopped. It preserves request timestamps and historical windows, writes each per-key snapshot atomically, and commits historical SQLite updates transactionally. Versioned records make a retry after interruption safe.

Current request records retain per-request input lengths, so model context thresholds can be applied. Old weekly summaries lack individual request lengths and service tiers; their per-model raw token totals are priced at Standard rates without inferring long-context premiums. Legacy cache-write counts were not retained. Legacy reasoning is normalized using provider semantics. Historical amounts are reconstructed estimates, not original provider invoices.

The three configured limit pairs convert to $4/$120, $120/$400, and $100/$1000. Keys without limits remain unlimited.

Example dry run on a private copy:

```bash
/tmp/cpa-migrate-client-billing-20260905 --dir /path/to/copied/usage-stats --config /path/to/copied/config.yaml --dry-run
```

The migration also accepts the same paths without `--dry-run` to apply conversion. Do not run it against an active writer.

## Backup and rollback prepared before deployment

Complete backup directory:

`/home/ubuntu/cliproxyapi/backups/usd-billing-20260905T060755Z`

Immediately before migration, stop the user service and create:

- `runtime.tar.gz`: all of `/home/ubuntu/cliproxyapi` except its existing backup directory, including binary, version, configuration, static HTML, and all usage JSON/SQLite state.
- `auth.tar.gz`: `/home/ubuntu/.cli-proxy-api` credentials.
- `systemd-user.tar.gz`: the user service configuration.
- `checksums.sha256` and `COMPLETE`: checksum verification and completed-backup marker.
- Current live binary/HTML are also copied to their matching `.good` files immediately before replacement.

The following is the concrete rollback command for this deployment:

```bash
bash '/home/ubuntu/cliproxyapi/backups/usd-billing-20260905T060755Z/rollback.sh'
```

The script first verifies the backup, stops CPA, preserves post-deploy USD data/config/artifacts in a timestamped subdirectory, restores the previous binary/config/panel/legacy usage together, restarts the **user** service, and checks health and panel HTTP responses. Rollback returns quotas to the pre-deployment snapshot; post-deploy usage is retained separately for reconciliation.

Authentication and systemd settings are backed up but not modified during normal deployment or rollback. If those files are independently damaged, restore the corresponding archives with the service stopped:

```bash
systemctl --user stop cliproxyapi.service
tar -xzf '/home/ubuntu/cliproxyapi/backups/usd-billing-20260905T060755Z/auth.tar.gz' -C /home/ubuntu
tar -xzf '/home/ubuntu/cliproxyapi/backups/usd-billing-20260905T060755Z/systemd-user.tar.gz' -C /home/ubuntu
systemctl --user daemon-reload
systemctl --user start cliproxyapi.service
```

## Verification gate

Before touching production: full Go tests, targeted race tests, frontend type/lint/tests/build, a migration rehearsal on copied live data, idempotence/restart tests including 100,000 events, and an isolated HTTP test of cost display/limit rejection. Backup restoration must also be rehearsed on a temporary directory before final installation. No Manager Server is installed; this host uses only CPA's lightweight panel.
