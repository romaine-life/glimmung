package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type reviewEvidenceCandidate struct {
	Artifact     EvidenceArtifact
	SourcePhase  string
	AttemptIndex int
}

// reviewEvidenceForRun normalizes the run's recorded verification evidence into
// review evidence and validates that every referenced artifact is durably
// present and well-typed in the artifact store.
//
// It is deliberately NOT a second evaluation of the evidence contract. The
// verification phase is the authority: it binds every required-evidence item by
// id to its observed evidence and renders the verdict (the spirelens guard
// EvidenceContract.ps1, ambience's verifier, etc.). A run only reaches review on
// an advancing pass verdict. The review gate's sole job is durability — not
// re-deriving a per-kind distinct-file count, which is a divergent, weaker
// invariant that the contract never specified and that falsely rejected a single
// artifact legitimately satisfying multiple same-kind requirements (one
// screenshot proving two on-screen facts, e.g. spirelens#147).
func reviewEvidenceForRun(ctx context.Context, artifactStore ArtifactStore, run RunReplayData) ([]ReviewEvidence, error) {
	candidates := evidenceCandidatesForRun(run)
	evidence := make([]ReviewEvidence, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		artifact := candidate.Artifact
		blobName, ok := artifactBlobNameForEvidence(run, artifact)
		if !ok || seen[blobName] {
			continue
		}
		seen[blobName] = true
		artifact.Kind = firstNonEmpty(NormalizeEvidenceKind(artifact.Kind), EvidenceKindForRef(blobName))
		if artifactStore != nil {
			if err := validateEvidenceArtifact(ctx, artifactStore, artifact.Kind, blobName); err != nil {
				return nil, err
			}
		}
		attemptIndex := candidate.AttemptIndex
		originalRef := firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL)
		ref := "blob://artifacts/" + blobName
		if strings.TrimSpace(artifact.Label) == "" {
			artifact.Label = evidenceLabel(originalRef)
		}
		artifact.Ref = ref
		if strings.TrimSpace(artifact.URL) == "" {
			artifact.URL = artifactURLForBlobName(blobName)
		}
		artifact.ArtifactPath = blobName
		artifact.SourcePhase = firstNonEmpty(strings.TrimSpace(artifact.SourcePhase), candidate.SourcePhase)
		if artifact.SourceAttemptIndex == nil {
			artifact.SourceAttemptIndex = &attemptIndex
		}
		evidence = append(evidence, ReviewEvidence{
			Kind:               artifact.Kind,
			Ref:                artifact.Ref,
			Label:              artifact.Label,
			URL:                artifact.URL,
			ArtifactPath:       artifact.ArtifactPath,
			ContentType:        artifact.ContentType,
			SizeBytes:          artifact.SizeBytes,
			DurationMS:         artifact.DurationMS,
			SourcePhase:        artifact.SourcePhase,
			SourceAttemptIndex: artifact.SourceAttemptIndex,
		})
	}
	return evidence, nil
}

func evidenceCandidatesForRun(run RunReplayData) []reviewEvidenceCandidate {
	candidates := make([]reviewEvidenceCandidate, 0)
	for _, attempt := range run.Attempts {
		if attempt.Verification != nil {
			for _, artifact := range attempt.Verification.Evidence {
				candidates = append(candidates, reviewEvidenceCandidate{
					Artifact:     artifact,
					SourcePhase:  attempt.Phase,
					AttemptIndex: attempt.AttemptIndex,
				})
			}
			for _, ref := range attempt.Verification.EvidenceRefs {
				candidates = append(candidates, reviewEvidenceCandidate{
					Artifact:     EvidenceArtifact{Ref: ref},
					SourcePhase:  attempt.Phase,
					AttemptIndex: attempt.AttemptIndex,
				})
			}
		}
	}
	return candidates
}

func stringSliceFromAny(raw any) []string {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s := strings.TrimSpace(stringValue(value))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func artifactBlobNameForEvidence(run RunReplayData, artifact EvidenceArtifact) (string, bool) {
	ref := firstNonEmpty(artifact.ArtifactPath, artifact.Ref, artifact.URL)
	artifactPath, ok := artifactPathFromEvidenceRef(ref)
	if !ok {
		artifactPath = strings.TrimSpace(ref)
		if strings.HasPrefix(artifactPath, "screenshots/") || strings.HasPrefix(artifactPath, "videos/") || strings.HasPrefix(artifactPath, "evidence/") || strings.HasPrefix(artifactPath, "inspections/") {
			if strings.TrimSpace(run.Project) == "" || strings.TrimSpace(run.ID) == "" {
				return "", false
			}
			artifactPath = "runs/" + strings.Trim(strings.TrimSpace(run.Project), "/") + "/" + strings.Trim(strings.TrimSpace(run.ID), "/") + "/" + strings.Trim(artifactPath, "/")
		}
	}
	artifactPath = trimEvidenceURLSuffix(artifactPath)
	if EvidenceKindForRef(artifactPath) == EvidenceKindArtifact && NormalizeEvidenceKind(artifact.Kind) == "" {
		return "", false
	}
	return servingArtifactBlobName(artifactPath)
}

func artifactPathFromEvidenceRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "blob://artifacts/"):
		return strings.TrimPrefix(ref, "blob://artifacts/"), true
	case strings.HasPrefix(ref, "/v1/artifacts/"):
		return unescapeArtifactPath(strings.TrimPrefix(ref, "/v1/artifacts/")), true
	case strings.HasPrefix(ref, "runs/"), strings.HasPrefix(ref, "issues/"), strings.HasPrefix(ref, "reports/"):
		return ref, true
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		parsed, err := url.Parse(ref)
		if err != nil {
			return "", false
		}
		idx := strings.Index(parsed.Path, "/v1/artifacts/")
		if idx < 0 {
			return "", false
		}
		return unescapeArtifactPath(parsed.Path[idx+len("/v1/artifacts/"):]), true
	default:
		return "", false
	}
}

func unescapeArtifactPath(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func trimEvidenceURLSuffix(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	return strings.Trim(value, "/")
}

func validateEvidenceArtifact(ctx context.Context, store ArtifactStore, kind, blobName string) error {
	artifact, err := store.Download(ctx, blobName)
	switch {
	case errors.Is(err, ErrArtifactNotFound):
		return ValidationError{Message: "evidence artifact not found: " + blobName}
	case err != nil:
		return fmt.Errorf("validate evidence artifact %q: %w", blobName, err)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(artifact.ContentType, ";")[0]))
	switch NormalizeEvidenceKind(kind) {
	case EvidenceKindScreenshot:
		if contentType != "" && !strings.HasPrefix(contentType, "image/") {
			return ValidationError{Message: fmt.Sprintf("screenshot artifact %s has non-image content type %q", blobName, artifact.ContentType)}
		}
	case EvidenceKindVideo:
		if contentType != "" && !strings.HasPrefix(contentType, "video/") {
			return ValidationError{Message: fmt.Sprintf("video artifact %s has non-video content type %q", blobName, artifact.ContentType)}
		}
		if err := videoEvidenceFirstFrameError(ctx, blobName, artifact.Body); err != nil {
			return err
		}
	}
	return nil
}

func artifactURLForBlobName(blobName string) string {
	parts := strings.Split(strings.Trim(blobName, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/v1/artifacts/" + strings.Join(parts, "/")
}
