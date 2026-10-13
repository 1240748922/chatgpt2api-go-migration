package httpapi

import "testing"

func TestOpenAIImageFormatPreservesPythonDefaultAndExplicitRequests(t *testing.T) {
	s := importTestServer(t)
	for _, f := range []string{"", "url", "b64_json"} {
		want := f
		if want == "" {
			want = "b64_json"
		}
		got, err := s.openAIImageResponseFormat(f)
		if err != nil || got != want {
			t.Fatalf("format %q got=%q err=%v", f, got, err)
		}
	}
	if _, err := s.store.PatchManagedConfig(map[string]any{"image_generation": map[string]any{"output_format": "url"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.openAIImageResponseFormat(""); err != nil || got != "url" {
		t.Fatal(got, err)
	}
	if got, err := s.openAIImageResponseFormat("b64_json"); err != nil || got != "b64_json" {
		t.Fatal(got, err)
	}
	if _, err := s.openAIImageResponseFormat("unsupported"); err == nil {
		t.Fatal("invalid format accepted")
	}
}
