package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"pansou/model"
)

func TestCheckConservativeClassification(t *testing.T) {
	cases := []struct{ name, disk, address, body, want string }{
		{"baidu empty response", "baidu", "https://pan.baidu.com/s/1example", `{}`, "uncertain"},
		{"baidu explicitly cancelled", "baidu", "https://pan.baidu.com/s/1example", `{"errno":-7}`, "bad"},
		{"quark missing detail", "quark", "https://pan.quark.cn/s/example", `{}`, "uncertain"},
		{"tianyi XML rate limit", "tianyi", "https://cloud.189.cn/t/example", `<error><code>RateLimit</code><message>too many requests</message></error>`, "uncertain"},
		{"tianyi token expired", "tianyi", "https://cloud.189.cn/t/example", `<error><code>TokenExpired</code><message>token过期</message></error>`, "uncertain"},
		{"tianyi unknown JSON error", "tianyi", "https://cloud.189.cn/t/example", `{"error_code":"InternalError","res_message":"retry later"}`, "uncertain"},
		{"tianyi explicit cancellation", "tianyi", "https://cloud.189.cn/t/example", `{"error_code":"ShareNotFound"}`, "bad"},
		{"123 unknown error", "123", "https://www.123pan.com/s/example", `{"code":500,"message":"系统繁忙"}`, "uncertain"},
		{"123 password", "123", "https://www.123pan.com/s/example", `{"code":1,"message":"提取码错误"}`, "locked"},
		{"123 invalid", "123", "https://www.123pan.com/s/example", `{"code":1,"message":"分享已取消"}`, "bad"},
		{"115 unknown error", "115", "https://115.com/s/example?password=abcd", `{"state":false,"error":"请求频繁"}`, "uncertain"},
		{"115 empty state", "115", "https://115.com/s/example?password=abcd", `{"state":true,"errno":0,"data":{}}`, "uncertain"},
		{"aliyun share password error", "aliyun", "https://www.alipan.com/s/example", `{"code":"ShareLinkPasswordMismatch","message":"密码错误"}`, "uncertain"},
		{"aliyun known expired", "aliyun", "https://www.alipan.com/s/example", `{"code":"ShareLink.Cancelled"}`, "bad"},
		{"aliyun empty contents", "aliyun", "https://www.alipan.com/s/example", `{"file_count":0}`, "uncertain"},
		{"xunlei token expired", "xunlei", "https://pan.xunlei.com/s/example", `{"error_code":1,"error_description":"token不存在"}`, "uncertain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: checkRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body := tc.body
				if tc.disk == "quark" && strings.Contains(req.URL.Path, "/token") {
					body = `{"code":0,"data":{"stoken":"example"}}`
				}
				if tc.disk == "xunlei" && strings.Contains(req.URL.Path, "captcha/init") {
					body = `{"captcha_token":"example"}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			s := &CheckService{}
			result, err := s.runCheck(context.Background(), model.CheckItem{DiskType: tc.disk, URL: tc.address}, tc.address, client)
			if err != nil || result.State != tc.want {
				t.Fatalf("got %+v err=%v want %s", result, err, tc.want)
			}
		})
	}
}

func TestCheckNormalizationPreservesShareFragments(t *testing.T) {
	s := &CheckService{}
	mobile := "https://yun.139.com/shareweb/#/w/i/example"
	if extractMobileShareID(s.normalizeShareLink("mobile", mobile, "")) != "example" {
		t.Fatal("mobile share identity lost")
	}
	tianyi := "https://cloud.189.cn/web/share?foo=bar#/t/example"
	code, _, _ := extractTianyiShareInfo(s.normalizeShareLink("tianyi", tianyi, ""), "")
	if code != "example" {
		t.Fatal("tianyi share identity lost")
	}
	_, password := extract115ShareInfo(s.normalizeShareLink("115", "https://115.com/s/example#password=abcd", ""), "")
	if password != "abcd" {
		t.Fatal("115 password fragment lost")
	}
	_, password = extractXunleiShareInfo(s.normalizeShareLink("xunlei", "https://pan.xunlei.com/s/example", "abcd"))
	if password != "abcd" {
		t.Fatal("xunlei separate password lost")
	}
}
