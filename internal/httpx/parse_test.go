package httpx

import "testing"

func TestTemplatesParse(t *testing.T) {
	if _, err := New(nil, "http://127.0.0.1", "dufaka.json"); err != nil {
		t.Fatal(err)
	}
}
