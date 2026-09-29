# Database Partition Management Runbook

## Overview
The `events` table is partitioned by timestamp range (`0017_soroban_events_partitioning.sql`). To prevent hard ingestion stops when future partitions run out, Trident automates partition creation and monitors partition bounds.

## Automatic Management
PostgreSQL background workers or indexer startup hooks call `ensure_future_event_partitions(months_ahead)` to maintain at least 3 months of future partitions ahead of time.

## Manual Partition Creation
If automation fails, create future partitions manually using:
```sql
SELECT ensure_future_event_partitions(3);
```

## Monitoring & Alerts
- **TridentPartitionExhaustionSoon**: Fires when the newest partition is within 7 days of its upper bound.
- **TridentPartitionExhaustionCritical**: Fires when the newest partition is within 2 days of its upper bound.
