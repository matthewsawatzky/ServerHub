package ssh

import "testing"

func TestParseImageLines(t *testing.T) {
	out := `{"repository":"nginx","tag":"latest","id":"sha256:abc123","size":"187MB","created":"2 weeks ago"}
{"repository":"<none>","tag":"<none>","id":"sha256:def456","size":"10MB","created":"3 months ago"}
{"repository":"bad","tag":"latest"}
not json
{"repository":"evil<script>","tag":"latest","id":"sha256:ghi789","size":"1MB","created":"now"}`
	images := ParseImageLines(out)
	if len(images) != 3 {
		t.Fatalf("got %d images, want 3: %+v", len(images), images)
	}
	if images[0].Repository != "nginx" || images[0].Tag != "latest" || images[0].ID != "sha256:abc123" {
		t.Fatalf("first image = %+v", images[0])
	}
	if images[1].Repository != "<none>" {
		t.Fatalf("dangling image = %+v", images[1])
	}
	// The frontend escapes data, but the boundary keeps the raw text bounded.
	if images[2].Repository != "evil<script>" {
		t.Fatalf("third image = %+v", images[2])
	}
}

func TestSanitizeImage(t *testing.T) {
	if !SanitizeImage(Image{Repository: "nginx", Tag: "latest", ID: "sha256:abc"}) {
		t.Fatal("expected a valid image")
	}
	if SanitizeImage(Image{Repository: "nginx", Tag: "latest"}) {
		t.Fatal("empty id should be rejected")
	}
	if SanitizeImage(Image{ID: "sha256:abc", Created: "bad\nvalue"}) {
		t.Fatal("control characters should be rejected")
	}
	if SanitizeImage(Image{ID: "sha256:abc", Repository: string(make([]byte, 300))}) {
		t.Fatal("over-long repository should be rejected")
	}
}

func TestValidContainerName(t *testing.T) {
	for _, name := range []string{"web", "my-app_1", "a.b-c"} {
		if !ValidContainerName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "-bad", "../etc/passwd", "a b", "x'; rm -rf /"} {
		if ValidContainerName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestInitTasksIncludesImages(t *testing.T) {
	e := &ServerEntry{}
	e.initTasks()
	found := false
	for _, task := range e.tasks {
		if task.name == "images" {
			found = true
			if task.interval != imageInterval || !task.skipOffline {
				t.Errorf("images task = %+v", task)
			}
		}
	}
	if !found {
		t.Fatal("images task missing from the poll registry")
	}
}
