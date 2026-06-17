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

func reviewEvidenceForRun(ctx context.Context, artifactStore ArtifactStore, run RunReplayData) ([]ReviewEvidence, error) {
	evidence, err := resolveReviewEvidence(ctx, artifactStore, run, nil)
	if err != nil {
		return nil, err
	}
	if err := checkRequiredReviewEvidence(run, evidence, artifactStore); err != nil {
		return nil, err
	}
	return evidence, nil
}

// reviewEvidenceCandidateResult is the per-candidate outcome of evidence
// resolution. The review preview (dry-run) surfaces these so an operator can
// see exactly which refs resolved to a durable, review-eligible artifact and
// why the rest were dropped — without finalizing a PR.
type reviewEvidenceCandidateResult struct {
	OriginalRef  string `json:"original_ref"`
	Kind         string `json:"kind"`
	SourcePhase  string `json:"source_phase"`
	AttemptIndex int    `json:"attempt_index"`
	Accepted     bool   `json:"accepted"`
	BlobName     string `json:"blob_name,omitempty"`
	URL          string `json:"url,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// resolveReviewEvidence turns the run's evidence candidates into the durable
// ReviewEvidence the review attaches. When diag is non-nil it also records a
// per-candidate result (accepted or the reason it was dropped). Resolution and
// the required-evidence check are deliberately separate so the dry-run preview
// can report partial evidence and the unmet requirement together instead of
// failing closed the way finalize does.
func resolveReviewEvidence(ctx context.Context, artifactStore ArtifactStore, run RunReplayData, diag *[]reviewEvidenceCandidateResult) ([]ReviewEvidence, error) {
	candidates := evidenceCandidatesForRun(run)
	evidence := make([]ReviewEvidence, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		artifact := candidate.Artifact
		originalRef := firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL)
		record := func(accepted bool, blobName, urlStr, reason string) {
			if diag == nil {
				return
			}
			*diag = append(*diag, reviewEvidenceCandidateResult{
				OriginalRef:  originalRef,
				Kind:         firstNonEmpty(NormalizeEvidenceKind(artifact.Kind), EvidenceKindForRef(firstNonEmpty(blobName, originalRef))),
				SourcePhase:  firstNonEmpty(strings.TrimSpace(artifact.SourcePhase), candidate.SourcePhase),
				AttemptIndex: candidate.AttemptIndex,
				Accepted:     accepted,
				BlobName:     blobName,
				URL:          urlStr,
				Reason:       reason,
			})
		}
		blobName, ok := artifactBlobNameForEvidence(run, artifact)
		if !ok {
			record(false, "", "", "ref does not resolve to a serveable run artifact")
			continue
		}
		if !runOwnsEvidenceArtifact(run, blobName) {
			// Provenance guard: review evidence must be the run's OWN
			// verification output (under runs/<project>/<run_id>/). A
			// lease-scoped inspections/<lease>/... browser capture or any other
			// run's artifact is not acceptable product evidence — this is what
			// made a /healthz screenshot reviewable. Finalize fails closed;
			// the preview records the rejection.
			reason := "evidence artifact is not this run's own verification output (must be under runs/" + strings.Trim(strings.TrimSpace(run.Project), "/") + "/" + strings.Trim(strings.TrimSpace(run.ID), "/") + "/)"
			if diag != nil {
				record(false, blobName, "", reason)
				continue
			}
			return nil, ValidationError{Message: reason + ": " + blobName}
		}
		if seen[blobName] {
			record(false, blobName, "", "duplicate of an already-resolved artifact")
			continue
		}
		seen[blobName] = true
		artifact.Kind = firstNonEmpty(NormalizeEvidenceKind(artifact.Kind), EvidenceKindForRef(blobName))
		if artifactStore != nil {
			if err := validateEvidenceArtifact(ctx, artifactStore, artifact.Kind, blobName); err != nil {
				// Finalize (diag == nil) fails closed on an unservable/invalid
				// artifact, preserving the strict contract. The preview
				// (diag != nil) instead records why the candidate was dropped
				// and keeps going, so an operator sees every problem at once.
				var validationErr ValidationError
				if diag != nil && errors.As(err, &validationErr) {
					record(false, blobName, "", validationErr.Message)
					continue
				}
				return nil, err
			}
		}
		attemptIndex := candidate.AttemptIndex
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
		record(true, blobName, artifact.URL, "")
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

// checkRequiredReviewEvidence enforces the test plan's required-evidence
// counts against the resolved evidence. Split out of reviewEvidenceForRun so
// the dry-run preview can report the same verdict without failing the request.
func checkRequiredReviewEvidence(run RunReplayData, evidence []ReviewEvidence, artifactStore ArtifactStore) error {
	requiredCounts := requiredEvidenceCounts(requiredEvidenceForRun(run))
	if len(requiredCounts) == 0 {
		return nil
	}
	if artifactStore == nil {
		return ValidationError{Message: "artifact store not configured for required evidence validation"}
	}
	actualCounts := map[string]int{}
	for _, item := range evidence {
		kind := firstNonEmpty(NormalizeEvidenceKind(item.Kind), EvidenceKindForRef(item.Ref))
		actualCounts[kind]++
	}
	for kind, count := range requiredCounts {
		if actualCounts[kind] == 0 {
			return ValidationError{Message: fmt.Sprintf("required %s evidence was not recorded", kind)}
		}
		if actualCounts[kind] < count {
			return ValidationError{Message: fmt.Sprintf("required %d %s evidence artifacts but only %d were recorded", count, kind, actualCounts[kind])}
		}
	}
	return nil
}

// ReviewEvidencePreview is the no-side-effect (dry-run) result of resolving a
// run's review evidence: exactly what the review would attach, plus a
// per-candidate breakdown and the required-vs-resolved verdict — without
// creating a PR or persisting a Review. It is the safe way to see review
// evidence without re-running the full workflow.
type ReviewEvidencePreview struct {
	RunRef           string                          `json:"run_ref"`
	Branch           string                          `json:"branch,omitempty"`
	Satisfied        bool                            `json:"satisfied"`
	Problem          string                          `json:"problem,omitempty"`
	RequiredEvidence map[string]int                  `json:"required_evidence,omitempty"`
	ResolvedEvidence []ReviewEvidence                `json:"resolved_evidence"`
	Candidates       []reviewEvidenceCandidateResult `json:"candidates"`
}

func reviewEvidencePreviewForRun(ctx context.Context, artifactStore ArtifactStore, run RunReplayData) (ReviewEvidencePreview, error) {
	var diag []reviewEvidenceCandidateResult
	evidence, err := resolveReviewEvidence(ctx, artifactStore, run, &diag)
	if err != nil {
		return ReviewEvidencePreview{}, err
	}
	preview := ReviewEvidencePreview{
		RequiredEvidence: requiredEvidenceCounts(requiredEvidenceForRun(run)),
		ResolvedEvidence: evidence,
		Candidates:       diag,
	}
	if reqErr := checkRequiredReviewEvidence(run, evidence, artifactStore); reqErr != nil {
		preview.Problem = reqErr.Error()
	} else {
		preview.Satisfied = true
	}
	return preview, nil
}

func requiredEvidenceForRun(run RunReplayData) []EvidenceRequirement {
	if len(run.EvidenceRequirements) > 0 {
		return run.EvidenceRequirements
	}
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(run.Attempts[i].PhaseOutputs["test_plan"])
		if raw == "" {
			continue
		}
		payload, ok := decodeEvidenceJSONOutputObject(raw)
		if !ok {
			continue
		}
		return MergeEvidenceRequirements(
			EvidenceRequirementsFromRaw(payload["required_evidence"]),
			EvidenceRequirementsFromRaw(payload["evidence_requirements"]),
		)
	}
	return nil
}

func requiredEvidenceCounts(requirements []EvidenceRequirement) map[string]int {
	counts := map[string]int{}
	for _, requirement := range requirements {
		if requirement.Optional {
			continue
		}
		kind := reviewArtifactRequirementKind(requirement.Kind)
		if kind == "" {
			kind = EvidenceKindVideo
		}
		if kind == "-" {
			continue
		}
		counts[kind]++
	}
	return counts
}

func reviewArtifactRequirementKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "":
		return ""
	case "screenshot", "image", "still":
		return EvidenceKindScreenshot
	case "video", "animation", "webm", "movie", "recording":
		return EvidenceKindVideo
	case "artifact", "file", "attachment":
		return EvidenceKindArtifact
	default:
		return "-"
	}
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
		raw := strings.TrimSpace(attempt.PhaseOutputs["verification"])
		if raw == "" {
			continue
		}
		for _, artifact := range EvidenceArtifactsFromVerificationOutput(raw) {
			candidates = append(candidates, reviewEvidenceCandidate{
				Artifact:     artifact,
				SourcePhase:  attempt.Phase,
				AttemptIndex: attempt.AttemptIndex,
			})
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

// runOwnsEvidenceArtifact reports whether a resolved evidence blob belongs to
// this run's own artifact namespace (runs/<project>/<run_id>/...). Run
// verification uploads — STS2 screenshots, recorded videos, run-scoped
// inspections — all live there. Lease-scoped inspections/<lease>/... captures
// and other runs' artifacts do not, and must not be accepted as this run's
// review evidence.
func runOwnsEvidenceArtifact(run RunReplayData, blobName string) bool {
	project := strings.Trim(strings.TrimSpace(run.Project), "/")
	runID := strings.Trim(strings.TrimSpace(run.ID), "/")
	if project == "" || runID == "" {
		return false
	}
	return strings.HasPrefix(blobName, "runs/"+project+"/"+runID+"/")
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
