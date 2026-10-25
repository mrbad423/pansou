package api

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"pansou/model"
	"pansou/service"
)

var searchLinkCheckerOnce sync.Once
var searchLinkChecker *service.SearchLinkChecker

func getSearchLinkChecker() *service.SearchLinkChecker {
	searchLinkCheckerOnce.Do(func() {
		searchLinkChecker = service.NewSearchLinkChecker(getCheckService())
	})
	return searchLinkChecker
}

func parseSearchCheckQuery(c *gin.Context, req *model.SearchRequest) error {
	for name, target := range map[string]**bool{
		"check_links":    &req.CheckLinks,
		"filter_invalid": &req.FilterInvalid,
	} {
		if value, present := c.GetQuery(name); present {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s 必须为 true 或 false", name)
			}
			*target = &parsed
		}
	}
	if value, present := c.GetQuery("check_limit"); present {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("check_limit 必须为 0 到 100 的整数")
		}
		req.CheckLimit = &parsed
	}
	return nil
}

func searchCheckOptions(req model.SearchRequest) (service.SearchCheckOptions, error) {
	options := service.DefaultSearchCheckOptions()
	if req.CheckLinks != nil {
		options.Enabled = *req.CheckLinks
	}
	if req.FilterInvalid != nil {
		options.FilterInvalid = *req.FilterInvalid
	}
	if req.CheckLimit != nil {
		if *req.CheckLimit < 0 || *req.CheckLimit > 100 {
			return options, fmt.Errorf("check_limit 必须为 0 到 100 的整数")
		}
		options.Limit = *req.CheckLimit
	}
	return options, nil
}
