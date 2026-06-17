package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingImageValidator struct {
	seen ResolvedTestSlotImage
	err  error
}

func (v *recordingImageValidator) ValidateTestSlotImage(_ context.Context, image ResolvedTestSlotImage) error {
	v.seen = image
	return v.err
}

func TestProjectMetadataTestSlotImageResolverResolvesFingerprintTagAndValidates(t *testing.T) {
	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
					"tags_by_sha": map[string]any{
						"abc123": "app-fingerprint123",
					},
				},
			},
		},
	}
	validator := &recordingImageValidator{}
	resolved, err := projectMetadataTestSlotImageResolver(validator)(context.Background(), project, "abc123")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Image != "romainecr.azurecr.io/tank-operator:app-fingerprint123" {
		t.Fatalf("image=%q", resolved.Image)
	}
	if validator.seen.Image != resolved.Image {
		t.Fatalf("validator saw %#v, want %#v", validator.seen, resolved)
	}
}

func TestProjectMetadataTestSlotImageResolverRejectsRawSHATag(t *testing.T) {
	sha := "abc123"
	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
					"tags_by_sha": map[string]any{
						sha: sha,
					},
				},
			},
		},
	}
	_, err := projectMetadataTestSlotImageResolver(noopTestSlotImageValidator{})(context.Background(), project, sha)
	if err == nil || !strings.Contains(err.Error(), "raw commit SHA") {
		t.Fatalf("err=%v, want raw commit SHA rejection", err)
	}
}

func TestProjectMetadataTestSlotImageResolverPropagatesValidationFailure(t *testing.T) {
	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"images_by_sha": map[string]any{
						"abc123": "romainecr.azurecr.io/tank-operator:app-fingerprint123",
					},
				},
			},
		},
	}
	want := errors.New("tag missing")
	_, err := projectMetadataTestSlotImageResolver(&recordingImageValidator{err: want})(context.Background(), project, "abc123")
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}
