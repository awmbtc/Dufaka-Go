package admin

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReadGroupNameLength(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "商品分类示例", wantErr: false}, // exactly six Chinese characters
		{name: "商品分类示例过长", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"gp_name": {tt.name},
				"is_open": {"1"},
				"ord":     {"1"},
			}
			r := httptest.NewRequest("POST", "/admin/groups", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			got, err := readGroup(r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readGroup(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if got.Name != tt.name {
				t.Fatalf("readGroup(%q) normalized name = %q", tt.name, got.Name)
			}
		})
	}
}

func TestReadGroupNameLengthCountsRunes(t *testing.T) {
	form := url.Values{
		"gp_name": {"商品A分类"}, // five Unicode runes, including one ASCII rune
		"is_open": {"1"},
		"ord":     {"1"},
	}
	r := httptest.NewRequest("POST", "/admin/groups", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := readGroup(r); err != nil {
		t.Fatalf("mixed name should be accepted within six runes: %v", err)
	}
}

func TestReadGroupNameLengthAllowsLatinBrandNames(t *testing.T) {
	for _, name := range []string{"苹果商店帐号", "GPT帐号", "Claude帐号"} {
		form := url.Values{
			"gp_name": {name},
			"is_open": {"1"},
			"ord":     {"1"},
		}
		r := httptest.NewRequest("POST", "/admin/groups", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if _, err := readGroup(r); err != nil {
			t.Fatalf("mixed category %q should be accepted: %v", name, err)
		}
	}
}
