package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"pansou/model"
)

type fakeSearchChecker struct {
	mu    sync.Mutex
	cache map[string]model.CheckResult
	check func(context.Context, model.CheckItem) model.CheckResult
}

func (f *fakeSearchChecker) searchCheckKey(item model.CheckItem) string {
	return item.DiskType + "|" + item.URL + "|" + item.Password
}
func (f *fakeSearchChecker) cachedSearchCheck(item model.CheckItem) (model.CheckResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result, ok := f.cache[f.searchCheckKey(item)]
	result.CacheHit = ok
	return result, ok
}
func (f *fakeSearchChecker) runSearchCheck(ctx context.Context, item model.CheckItem) model.CheckResult {
	result := f.check(ctx, item)
	f.mu.Lock()
	f.cache[f.searchCheckKey(item)] = result
	f.mu.Unlock()
	return result
}
func newFakeSearchChecker() *fakeSearchChecker {
	return &fakeSearchChecker{cache: make(map[string]model.CheckResult)}
}
func optionsForTest() SearchCheckOptions {
	return SearchCheckOptions{Enabled: true, FilterInvalid: true, Limit: 20, Budget: time.Second}
}

func TestSearchCheckFiltersCopiesAndCounts(t *testing.T) {
	for _, shape := range []string{"merged_by_type", "results", "all"} {
		t.Run(shape, func(t *testing.T) {
			fake := newFakeSearchChecker()
			states := []string{"bad", "ok", "locked", "uncertain", "unsupported"}
			response := model.SearchResponse{MergedByType: model.MergedLinks{"quark": {}}, Results: []model.SearchResult{{Title: "one"}}}
			for _, state := range states {
				item := model.CheckItem{DiskType: "quark", URL: state}
				fake.cache[fake.searchCheckKey(item)] = model.CheckResult{State: state, URL: state}
				response.MergedByType["quark"] = append(response.MergedByType["quark"], model.MergedLink{URL: state})
				response.Results[0].Links = append(response.Results[0].Links, model.Link{Type: "quark", URL: state})
			}
			if shape == "results" {
				response.MergedByType = nil
			}
			if shape == "merged_by_type" {
				response.Results = nil
			}
			original, _ := json.Marshal(response)
			checker := newSearchLinkChecker(fake)
			defer checker.Close()
			result := checker.Apply(context.Background(), response, shape, optionsForTest())
			if result.LinkCheck.TotalLinks != 5 || result.LinkCheck.Filtered != 1 || result.LinkCheck.CacheHits != 5 {
				t.Fatalf("summary: %+v", result.LinkCheck)
			}
			if shape != "results" && len(result.MergedByType["quark"]) != 4 {
				t.Fatalf("merged links: %+v", result.MergedByType)
			}
			if shape != "merged_by_type" && len(result.Results[0].Links) != 4 {
				t.Fatalf("raw links: %+v", result.Results)
			}
			wantTotal := 1
			if shape == "merged_by_type" {
				wantTotal = 4
			}
			if result.Total != wantTotal {
				t.Fatalf("total=%d want %d", result.Total, wantTotal)
			}
			after, _ := json.Marshal(response)
			if string(original) != string(after) {
				t.Fatal("mutated raw search/cache collections")
			}
			options := optionsForTest()
			options.FilterInvalid = false
			marked := checker.Apply(context.Background(), response, shape, options)
			if marked.LinkCheck.Filtered != 0 {
				t.Fatal("mark-only removed links")
			}
			options.Enabled = false
			if disabled := checker.Apply(context.Background(), response, shape, options); !reflect.DeepEqual(disabled, response) {
				t.Fatal("disabled mode changed response")
			}
		})
	}
}

func TestSearchCheckDeadlineAndBackgroundCache(t *testing.T) {
	fake := newFakeSearchChecker()
	release := make(chan struct{})
	fastDone := make(chan struct{})
	fake.check = func(ctx context.Context, item model.CheckItem) model.CheckResult {
		if item.URL == "slow" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		} else {
			close(fastDone)
		}
		return model.CheckResult{DiskType: item.DiskType, URL: item.URL, State: "bad"}
	}
	checker := newSearchLinkChecker(fake)
	defer checker.Close()
	response := model.SearchResponse{MergedByType: model.MergedLinks{"quark": {{URL: "slow"}, {URL: "fast"}}}}
	options := optionsForTest()
	options.Budget = 150 * time.Millisecond
	started := time.Now()
	result := checker.Apply(context.Background(), response, "merged_by_type", options)
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("missed shared budget: %s", elapsed)
	}
	select {
	case <-fastDone:
	default:
		t.Fatal("slow item prevented concurrent fast detection")
	}
	if len(result.MergedByType["quark"]) != 1 || result.MergedByType["quark"][0].URL != "slow" {
		t.Fatalf("wrong retained links: %+v", result)
	}
	if result.LinkCheck.Filtered != 1 || result.LinkCheck.Unchecked != 1 || !result.LinkCheck.BudgetExhausted {
		t.Fatalf("summary %+v", result.LinkCheck)
	}
	// An HTTP deadline must not kill the bounded background job.
	job := checker.submit(model.CheckItem{DiskType: "quark", URL: "slow"})
	close(release)
	select {
	case <-job.done:
	case <-time.After(time.Second):
		t.Fatal("background job did not finish")
	}
	options.Limit = 0
	result = checker.Apply(context.Background(), response, "merged_by_type", options)
	if result.Total != 0 || result.LinkCheck.Filtered != 2 || result.LinkCheck.CacheHits != 2 {
		t.Fatalf("background cache not reused: %+v", result)
	}
}

func TestSearchCheckPoolIsBoundedAndFair(t *testing.T) {
	fake := newFakeSearchChecker()
	release := make(chan struct{})
	started := make(chan struct{}, searchCheckCapacity)
	var mu sync.Mutex
	active, peak := 0, 0
	byDisk := map[string]int{}
	fake.check = func(ctx context.Context, item model.CheckItem) model.CheckResult {
		mu.Lock()
		active++
		byDisk[item.DiskType]++
		if active > peak {
			peak = active
		}
		if byDisk[item.DiskType] > searchCheckPerDisk {
			t.Errorf("per-disk limit exceeded: %s", item.DiskType)
		}
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		active--
		byDisk[item.DiskType]--
		mu.Unlock()
		return model.CheckResult{State: "ok"}
	}
	checker := newSearchLinkChecker(fake)
	defer checker.Close()
	jobs := make([]*searchCheckJob, 0, searchCheckCapacity)
	// Queue a slow disk first; subsequent disks must still reach all 8 workers.
	for i := 0; i < searchCheckCapacity; i++ {
		disk := "quark"
		if i >= 20 {
			disk = fmt.Sprintf("disk-%d", i%5)
		}
		job := checker.submit(model.CheckItem{DiskType: disk, URL: fmt.Sprint(i)})
		if job == nil {
			t.Fatal("queue rejected within capacity")
		}
		jobs = append(jobs, job)
	}
	if extra := checker.submit(model.CheckItem{DiskType: "baidu", URL: "overflow"}); extra != nil {
		t.Fatal("queue accepted overflow")
	}
	if duplicate := checker.submit(model.CheckItem{DiskType: "quark", URL: "0"}); duplicate != jobs[0] {
		t.Fatal("same link not coalesced")
	}
	for i := 0; i < searchCheckWorkers; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("per-disk head-of-line blocking")
		}
	}
	mu.Lock()
	if peak != searchCheckWorkers {
		t.Errorf("peak=%d", peak)
	}
	mu.Unlock()
	close(release)
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-time.After(2 * time.Second):
			t.Fatal("job not drained")
		}
	}
	mu.Lock()
	if peak > searchCheckWorkers {
		t.Errorf("global limit exceeded: %d", peak)
	}
	mu.Unlock()
}

func TestSearchCheckLiveLimitAndUnsupported(t *testing.T) {
	fake := newFakeSearchChecker()
	fake.check = func(ctx context.Context, item model.CheckItem) model.CheckResult {
		return model.CheckResult{State: "ok"}
	}
	checker := newSearchLinkChecker(fake)
	defer checker.Close()
	response := model.SearchResponse{MergedByType: model.MergedLinks{"quark": {{URL: "one"}, {URL: "two"}}, "magnet": {{URL: "magnet:?xt=example"}}}}
	options := optionsForTest()
	options.Limit = 1
	result := checker.Apply(context.Background(), response, "merged_by_type", options)
	if result.LinkCheck.Checked != 2 || result.LinkCheck.Unsupported != 1 || result.LinkCheck.Unchecked != 1 || result.Total != 3 {
		t.Fatalf("summary %+v", result.LinkCheck)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	options.Limit = 100
	result = checker.Apply(ctx, response, "merged_by_type", options)
	if result.Total != 3 {
		t.Fatal("cancellation removed unverified links")
	}
}

func TestSearchCheckCacheIdentity(t *testing.T) {
	s := &CheckService{}
	base := model.CheckItem{DiskType: "115", URL: "https://115.com/s/example", Password: "one"}
	other := base
	other.Password = "two"
	if s.searchCheckKey(base) == s.searchCheckKey(other) {
		t.Fatal("passwords share cache identity")
	}
	key := checkItemCacheKey(base, base.URL, "")
	if key == checkItemCacheKey(base, base.URL, proxyCacheScope("socks5://example:1234")) {
		t.Fatal("proxies share cache identity")
	}
}

func TestSearchCheckCancellationReachesHTTP(t *testing.T) {
	client := &http.Client{Transport: checkRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	s := &CheckService{client: client, cache: make(map[string]cachedCheckResult), inflight: make(map[string]*activeCheckCall)}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := s.runSearchCheck(ctx, model.CheckItem{DiskType: "quark", URL: "https://pan.quark.cn/s/example"})
	if result.State != "uncertain" || time.Since(started) > time.Second {
		t.Fatalf("cancellation not propagated: %+v", result)
	}
}

func TestCheckHTTPFailuresNeverBecomeBad(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: checkRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"code":41004,"message":"链接失效"}`)), Header: make(http.Header)}, nil
			})}
			s := &CheckService{client: client, cache: make(map[string]cachedCheckResult), inflight: make(map[string]*activeCheckCall)}
			result := s.runSearchCheck(context.Background(), model.CheckItem{DiskType: "quark", URL: "https://pan.quark.cn/s/example"})
			if result.State != "uncertain" {
				t.Fatalf("HTTP %d misclassified: %+v", status, result)
			}
		})
	}
}

func TestCheckCacheUsesPersistentDataDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CACHE_PATH", dir)
	s := NewCheckService()
	if s.cacheDB == nil {
		t.Fatal("cache DB not opened")
	}
	item := model.CheckItem{DiskType: "quark", URL: "https://pan.quark.cn/s/example", Password: "abcd"}
	normalized := s.normalizeShareLink(item.DiskType, item.URL, item.Password)
	result := s.buildResult(item, normalized, "bad", false, "链接已失效")
	key := s.searchCheckKey(item)
	s.savePersistentCache(key, cachedCheckResult{result: result, expiresAt: time.UnixMilli(result.ExpiresAt)})
	if err := s.cacheDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewCheckService()
	defer reopened.cacheDB.Close()
	cached, ok := reopened.cachedSearchCheck(item)
	if !ok || cached.State != "bad" || !cached.CacheHit {
		t.Fatalf("cache did not survive reopen: %+v", cached)
	}
}
