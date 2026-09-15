package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestImagesMainModelRuntimePrecedenceAndResponses(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	t.Setenv(imagesMainModelEnv, " gpt-5.5 ")
	ApplyRuntimeSettingsFromSystem(&database.SystemSettings{CodexImagesMainModel: " gpt-5.6-terra "})
	if imagesMainModel() != "gpt-5.6-terra" || ImagesDefaultMainModel() != "gpt-5.5" {
		t.Fatal("persisted image driver should take precedence over the deployment default")
	}
	for _, prepare := range []func([]byte) ([]byte, string){PrepareResponsesBody, PrepareResponsesWebSocketBody} {
		for _, model := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-5.6-sol"} {
			body, _ := prepare([]byte(fmt.Sprintf(`{"model":%q,"input":"draw","tools":[{"type":"image_generation","model":"gpt-image-2.5-sunburst","quality":"max"}]}`, model)))
			want := "gpt-5.6-terra"
			if model == "gpt-5.6-sol" {
				want = model
			}
			if gjson.GetBytes(body, "model").String() != want || gjson.GetBytes(body, "tools.0.model").String() != "gpt-image-2.5-sunburst" || gjson.GetBytes(body, "tools.0.quality").String() != "max" {
				t.Fatalf("Responses model/quality mismatch: %s", body)
			}
		}
	}
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.CodexImagesMainModel = ""
		return current
	})
	if imagesMainModel() != "gpt-5.5" {
		t.Fatal("clearing the runtime setting should restore the environment default")
	}
	t.Setenv(imagesMainModelEnv, "")
	if imagesMainModel() != defaultImagesMainModel {
		t.Fatal("empty settings and environment should use the built-in default")
	}
}

func TestNormalizeImagesMainModel(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "GPT-IMAGE-2.5-FLARE-4k", "gpt-5.6 sol", "gpt-5.6\nsol", "gpt-5.6\x00sol", "gpt-5.6\u3000sol", strings.Repeat("a", 129)} {
		if _, err := NormalizeImagesMainModel(model); err == nil {
			t.Errorf("accepted invalid driver %q", model)
		}
	}
	for _, model := range []string{"", "  ", " gpt-5.6-sol ", "relay/new-text-model"} {
		if got, err := NormalizeImagesMainModel(model); err != nil || got != strings.TrimSpace(model) {
			t.Errorf("NormalizeImagesMainModel(%q) = %q, %v", model, got, err)
		}
	}
}
