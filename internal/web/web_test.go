package web

import "testing"

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/p/12": true, "/setup?x=1": true,
		"": false, "//evil.example": false, "/\\evil.example": false, "/\t/evil.example": false,
		"/\n/evil.example": false, "/\r/evil.example": false, "/\x7f": false, "https://evil.example": false, "evil.example": false,
	} {
		if got := localPath(p); got != want {
			t.Errorf("localPath(%q) = %v, want %v", p, got, want)
		}
	}
}
