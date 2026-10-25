package model

// SearchLinkCheckSummary counts unique (disk, URL, password) identities, not
// duplicate appearances in results and merged_by_type. checked includes cached
// uncertain/unsupported outcomes; unchecked links are retained in the response.
type SearchLinkCheckSummary struct {
	TotalLinks      int   `json:"total_links"`
	Checked         int   `json:"checked"`
	CacheHits       int   `json:"cache_hits"`
	Filtered        int   `json:"filtered"`
	Unchecked       int   `json:"unchecked"`
	Uncertain       int   `json:"uncertain"`
	Unsupported     int   `json:"unsupported"`
	DurationMS      int64 `json:"duration_ms"`
	BudgetExhausted bool  `json:"budget_exhausted"`
}
