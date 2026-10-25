package service

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pansou/model"
)

const (
	searchCheckWorkers  = 8
	searchCheckPerDisk  = 2
	searchCheckCapacity = 100              // queued + running; shared across all searches
	searchCheckLifetime = 12 * time.Second // includes queue time
)

type SearchCheckOptions struct {
	Enabled       bool
	FilterInvalid bool
	Limit         int
	Budget        time.Duration
}

func DefaultSearchCheckOptions() SearchCheckOptions {
	return SearchCheckOptions{
		Enabled:       envSearchCheckBool("SEARCH_CHECK_LINKS", true),
		FilterInvalid: envSearchCheckBool("SEARCH_FILTER_INVALID", true),
		Limit:         envSearchCheckInt("SEARCH_CHECK_LIMIT", 20, 0, 100),
		Budget:        time.Duration(envSearchCheckInt("SEARCH_CHECK_BUDGET_MS", 2000, 0, 5000)) * time.Millisecond,
	}
}

func envSearchCheckBool(name string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func envSearchCheckInt(name string, fallback, low, high int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < low || value > high {
		return fallback
	}
	return value
}

type searchCheckBackend interface {
	searchCheckKey(model.CheckItem) string
	cachedSearchCheck(model.CheckItem) (model.CheckResult, bool)
	runSearchCheck(context.Context, model.CheckItem) model.CheckResult
}

func (s *CheckService) searchCheckKey(item model.CheckItem) string {
	return checkItemCacheKey(item, s.normalizeShareLink(item.DiskType, item.URL, item.Password), "")
}

func (s *CheckService) cachedSearchCheck(item model.CheckItem) (model.CheckResult, bool) {
	result, ok := s.getCached(s.searchCheckKey(item))
	if ok {
		result.CacheHit = true
		result.URL = item.URL
	}
	return result, ok
}

func (s *CheckService) runSearchCheck(ctx context.Context, item model.CheckItem) model.CheckResult {
	return s.checkOneContext(ctx, item, s.client, "")
}

type searchCheckJob struct {
	item    model.CheckItem
	key     string
	expires time.Time
	done    chan struct{}
	result  model.CheckResult // written once, published by closing done
}

// SearchLinkChecker owns a bounded process-wide queue. A slow disk does not
// occupy workers waiting for its per-disk limit: workers select eligible jobs.
// Jobs may outlive the HTTP wait budget, but never their own bounded lifetime.
type SearchLinkChecker struct {
	backend searchCheckBackend
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*searchCheckJob
	jobs    map[string]*searchCheckJob
	active  map[string]int
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	workers sync.WaitGroup
}

func NewSearchLinkChecker(checker *CheckService) *SearchLinkChecker {
	return newSearchLinkChecker(checker)
}

func newSearchLinkChecker(backend searchCheckBackend) *SearchLinkChecker {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SearchLinkChecker{backend: backend, jobs: make(map[string]*searchCheckJob), active: make(map[string]int), ctx: ctx, cancel: cancel}
	s.cond = sync.NewCond(&s.mu)
	for i := 0; i < searchCheckWorkers; i++ {
		s.workers.Add(1)
		go s.worker()
	}
	return s
}

func (s *SearchLinkChecker) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	for _, job := range s.queue {
		job.result = model.CheckResult{DiskType: job.item.DiskType, URL: job.item.URL, State: checkStateUncertain, Summary: "检测已取消"}
		delete(s.jobs, job.key)
		close(job.done)
	}
	s.queue = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *SearchLinkChecker) submit(item model.CheckItem) *searchCheckJob {
	key := s.backend.searchCheckKey(item)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if job := s.jobs[key]; job != nil {
		return job
	}
	if len(s.jobs) >= searchCheckCapacity {
		return nil
	}
	job := &searchCheckJob{item: item, key: key, expires: time.Now().Add(searchCheckLifetime), done: make(chan struct{})}
	s.jobs[key] = job
	s.queue = append(s.queue, job)
	s.cond.Broadcast()
	return job
}

func (s *SearchLinkChecker) nextJob() *searchCheckJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		for i, job := range s.queue {
			if s.active[job.item.DiskType] >= searchCheckPerDisk {
				continue
			}
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.active[job.item.DiskType]++
			return job
		}
		s.cond.Wait()
	}
	return nil
}

func (s *SearchLinkChecker) worker() {
	defer s.workers.Done()
	for {
		job := s.nextJob()
		if job == nil {
			return
		}
		ctx, cancel := context.WithDeadline(s.ctx, job.expires)
		if ctx.Err() != nil {
			job.result = model.CheckResult{DiskType: job.item.DiskType, URL: job.item.URL, State: checkStateUncertain, Summary: "检测队列等待超时"}
		} else {
			job.result = s.backend.runSearchCheck(ctx, job.item)
		}
		cancel()
		s.mu.Lock()
		s.active[job.item.DiskType]--
		delete(s.jobs, job.key)
		close(job.done)
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

func supportedSearchCheckDisk(disk string) bool {
	switch disk {
	case "aliyun", "quark", "uc", "baidu", "tianyi", "123", "xunlei", "115", "mobile":
		return true
	}
	return false
}

// Apply adds health metadata to copies of the response collections. It never
// mutates the raw search cache or the input slices/maps. Budget bounds network
// waiting; normal result traversal/JSON encoding remains proportional to size.
func (s *SearchLinkChecker) Apply(parent context.Context, response model.SearchResponse, resultType string, options SearchCheckOptions) model.SearchResponse {
	if !options.Enabled {
		return response
	}
	started := time.Now()
	if options.Limit < 0 {
		options.Limit = 0
	}
	if options.Limit > 100 {
		options.Limit = 100
	}
	if options.Budget < 0 {
		options.Budget = 0
	}
	if options.Budget > 5*time.Second {
		options.Budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, options.Budget)
	defer cancel()
	items := s.collectItems(response)
	outcomes := make(map[string]model.CheckResult, len(items))
	waiting := make(map[string]*searchCheckJob)
	attempted := 0
	for _, item := range items {
		key := s.backend.searchCheckKey(item)
		if result, ok := s.backend.cachedSearchCheck(item); ok {
			outcomes[key] = result
			continue
		}
		if !supportedSearchCheckDisk(item.DiskType) {
			outcomes[key] = model.CheckResult{DiskType: item.DiskType, URL: item.URL, State: checkStateUnsupported, Summary: "当前平台暂不支持检测"}
			continue
		}
		if attempted >= options.Limit || ctx.Err() != nil {
			continue
		}
		attempted++
		if job := s.submit(item); job != nil {
			waiting[key] = job
		}
	}
	// One shared deadline, not a fresh timeout for each link. Completed jobs are
	// collected again below so a slow first job cannot hide later fast results.
	for _, job := range waiting {
		select {
		case <-ctx.Done():
		case <-job.done:
		}
		if ctx.Err() != nil {
			break
		}
	}
	for key, job := range waiting {
		select {
		case <-job.done:
			outcomes[key] = job.result
		default:
		}
	}
	summary := &model.SearchLinkCheckSummary{TotalLinks: len(items), DurationMS: time.Since(started).Milliseconds()}
	for _, item := range items {
		key := s.backend.searchCheckKey(item)
		result, ok := outcomes[key]
		if !ok {
			summary.Unchecked++
			outcomes[key] = model.CheckResult{DiskType: item.DiskType, URL: item.URL, State: "unchecked", Summary: "未完成检测：数量、队列或时间预算限制"}
			continue
		}
		summary.Checked++
		if result.CacheHit {
			summary.CacheHits++
		}
		switch result.State {
		case checkStateBad:
			if options.FilterInvalid {
				summary.Filtered++
			}
		case checkStateUncertain:
			summary.Uncertain++
		case checkStateUnsupported:
			summary.Unsupported++
		}
	}
	if ctx.Err() != nil {
		for key := range waiting {
			if outcomes[key].State == "unchecked" {
				summary.BudgetExhausted = true
				break
			}
		}
	}
	response = s.annotateAndFilter(response, resultType, outcomes, options.FilterInvalid)
	response.LinkCheck = summary
	return response
}

// For merged-only responses, round-robin over sorted disk names to avoid Go map
// randomness or a single platform consuming the entire live-check allowance.
func (s *SearchLinkChecker) collectItems(response model.SearchResponse) []model.CheckItem {
	items := make([]model.CheckItem, 0)
	seen := make(map[string]bool)
	add := func(item model.CheckItem) {
		item.DiskType = strings.ToLower(strings.TrimSpace(item.DiskType))
		key := s.backend.searchCheckKey(item)
		if !seen[key] {
			seen[key] = true
			items = append(items, item)
		}
	}
	for _, result := range response.Results {
		for _, link := range result.Links {
			add(model.CheckItem{DiskType: link.Type, URL: link.URL, Password: link.Password})
		}
	}
	disks := make([]string, 0, len(response.MergedByType))
	for disk := range response.MergedByType {
		disks = append(disks, disk)
	}
	sort.Strings(disks)
	for index := 0; ; index++ {
		found := false
		for _, disk := range disks {
			links := response.MergedByType[disk]
			if index < len(links) {
				found = true
				link := links[index]
				add(model.CheckItem{DiskType: disk, URL: link.URL, Password: link.Password})
			}
		}
		if !found {
			break
		}
	}
	return items
}

func (s *SearchLinkChecker) annotateAndFilter(response model.SearchResponse, resultType string, outcomes map[string]model.CheckResult, filter bool) model.SearchResponse {
	lookup := func(disk, address, password string) model.CheckResult {
		item := model.CheckItem{DiskType: strings.ToLower(strings.TrimSpace(disk)), URL: address, Password: password}
		result := outcomes[s.backend.searchCheckKey(item)]
		result.URL = address
		return result
	}
	if response.Results != nil {
		results := make([]model.SearchResult, 0, len(response.Results))
		for _, result := range response.Results {
			links := make([]model.Link, 0, len(result.Links))
			for _, link := range result.Links {
				check := lookup(link.Type, link.URL, link.Password)
				if filter && check.State == checkStateBad {
					continue
				}
				link.LinkCheck = &check
				links = append(links, link)
			}
			if len(result.Links) > 0 && len(links) == 0 {
				continue
			}
			result.Links = links
			results = append(results, result)
		}
		response.Results = results
	}
	if response.MergedByType != nil {
		merged := make(model.MergedLinks, len(response.MergedByType))
		for disk, original := range response.MergedByType {
			links := make([]model.MergedLink, 0, len(original))
			for _, link := range original {
				check := lookup(disk, link.URL, link.Password)
				if filter && check.State == checkStateBad {
					continue
				}
				link.LinkCheck = &check
				links = append(links, link)
			}
			if len(links) > 0 {
				merged[disk] = links
			}
		}
		response.MergedByType = merged
	}
	if resultType == "all" || resultType == "results" {
		response.Total = len(response.Results)
	} else {
		response.Total = 0
		for _, links := range response.MergedByType {
			response.Total += len(links)
		}
	}
	return response
}
