package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"pansou/model"
)

func TestSearchCheckGETAndPOSTOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var post model.SearchRequest
	if err := json.Unmarshal([]byte(`{"kw":"example","check_links":false,"filter_invalid":false,"check_limit":0}`), &post); err != nil {
		t.Fatal(err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/search?kw=example&check_links=false&filter_invalid=false&check_limit=0", nil)
	var get model.SearchRequest
	if err := parseSearchCheckQuery(c, &get); err != nil {
		t.Fatal(err)
	}
	for _, req := range []model.SearchRequest{post, get} {
		options, err := searchCheckOptions(req)
		if err != nil || options.Enabled || options.FilterInvalid || options.Limit != 0 {
			t.Fatalf("options %+v, err=%v", options, err)
		}
	}
	t.Setenv("SEARCH_CHECK_LINKS", "true")
	t.Setenv("SEARCH_FILTER_INVALID", "true")
	t.Setenv("SEARCH_CHECK_LIMIT", "20")
	t.Setenv("SEARCH_CHECK_BUDGET_MS", "2000")
	options, err := searchCheckOptions(model.SearchRequest{})
	if err != nil || !options.Enabled || !options.FilterInvalid || options.Limit != 20 || options.Budget.Milliseconds() != 2000 {
		t.Fatalf("defaults %+v err=%v", options, err)
	}
}

func TestSearchCheckRejectsBadOptionsBeforeSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/search?kw=test&check_links=maybe", ""},
		{"GET", "/api/search?kw=test&check_limit=-1", ""},
		{"GET", "/api/search?kw=test&check_limit=101", ""},
		{"POST", "/api/search", `{"kw":"test","check_limit":101}`},
		{"POST", "/api/search", `{"kw":"test","check_links":"false"}`},
	} {
		router := gin.New()
		router.GET("/api/search", SearchHandler)
		router.POST("/api/search", SearchHandler)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
}
