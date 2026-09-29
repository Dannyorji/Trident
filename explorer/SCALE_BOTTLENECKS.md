# Scale Bottlenecks & Fixes

## Identified Issues

### 1. Home Ticker Polling (High Priority)
**Issue**: `/api/recent-events.json` fetches every 5 seconds client-side with no cache.
- 1,440 requests per day per user
- At scale (1000 concurrent users) = 1.44M requests/day
- API rate limit: 60 req/min on free tier = 86,400 req/day

**Impact**: Will hit rate limit quickly, ticker becomes unreliable.

**Fix**: 
- Add cache header: `Cache-Control: public, max-age=5` 
- Server-side deduplication (deduplicate 5s window)
- OR: Reduce polling to 10s instead of 5s
- OR: Use WebSocket for live updates (future)

**Status**: ✅ FIXED (see recent-events.json cache fix)

---

### 2. Contract Page: Distinct Topic Computation
**Issue**: `distinctTypes` computed from only the 25 events on current page.
- With filters (topic0, ledgerRange), "All types" filter becomes misleading
- User sees 5 topic types but there are 50+ globally for the contract
- No way to discover all event types without manual pagination

**Impact**: Confusing UX + users can't find events they know exist.

**Fix**:
- Add new API endpoint: `GET /v1/events/distinct-topics?contractId=...`
- Cache the result (rarely changes)
- Return top 10 + "show more" if 10+
- Update contract page to call this endpoint server-side

**Status**: 📋 PENDING (scope expanded API)

---

### 3. Contract Page: Large Tables at Scale
**Issue**: Tables render 25 rows per page. No virtual scrolling or lazy rendering.
- At 25 rows: acceptable (< 50ms render)
- At 100+ rows (if limit increases): slow (> 500ms render)
- Possible future: batch loading all rows into memory

**Impact**: Page becomes sluggish if someone tries to load all events.

**Fix**:
- Keep default 25 per page (good balance)
- Add option to show 50 per page with explicit "This may be slow" warning
- OR: Implement virtual scrolling (Astro + JS)
- For now: document that 25 is the sweet spot

**Status**: ✅ FIXED (keep 25 default, document limit)

---

### 4. API Query Filters Without Indexes
**Issue**: Backend API may not have indexes on:
- `contract_id` + `topic0` (combined)
- `ledger_sequence` range queries
- Cursor pagination may not be optimized

**Impact**: 500ms+ query latency at scale.

**Fix**:
- This is backend infrastructure, not explorer code
- Document in PERFORMANCE_BUDGET that backend must have:
  - Index on `contract_id`
  - Index on `(contract_id, topic0)`
  - Index on `ledger_sequence` for range queries
  - Cursor field indexed for pagination

**Status**: 📋 DEPENDS ON BACKEND

---

## Priority Fixes (Explorer-Side)

1. **Add Cache-Control to API routes** ✅
2. **Reduce ticker polling to 10s** ✅
3. **Document max page size** ✅
4. **Add N+1 query guard to prevent future regressions** 📋

