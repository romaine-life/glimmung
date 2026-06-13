package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// upload-evidence is a Glimmung-managed primitive invoked as a STEP inside the
// verification job's own pod (canonicalization auto-appends the step; see
// internal/server/evidence_gate.go and docs/design/evidence-upload-primitive.md).
// It replaces the hand-rolled `az storage blob upload-batch --auth-mode login`
// project bash (which needed an explicit `az login --service-principal
// --federated-token …` first — the exact login spirelens dropped). Here the
// upload authenticates with the Azure SDK credential chain
// (azidentity.NewDefaultAzureCredential), which consumes the projected
// workload-identity federated token (AZURE_FEDERATED_TOKEN_FILE / AZURE_CLIENT_ID
// / AZURE_TENANT_ID) directly — no `az` CLI, no `az login`.

// evidenceBlobClient is the minimal blob surface the upload needs, abstracted so
// the walk + prefix + empty-dir logic is unit-testable without real Azure.
type evidenceBlobClient interface {
	UploadBuffer(ctx context.Context, container, blobName string, body []byte, contentType string) error
}

// plannedUpload is one (object key, bytes) PUT the walk would perform. It is the
// observable contract the table test asserts against.
type plannedUpload struct {
	BlobName    string
	Body        []byte
	ContentType string
}

// uploadEvidenceConfig captures the environment the primitive reads. It mirrors
// the env the current project bash consumes: the storage account/container come
// from AGENT_SCREENSHOT_STORAGE_ACCOUNT / AGENT_SCREENSHOT_CONTAINER (injected by
// the project's job env, the same source the bash uses — not hardcoded), and the
// run-scoped prefix is <project>/<run-ref>.
type uploadEvidenceConfig struct {
	EvidenceDir    string
	StorageAccount string
	Container      string
	Project        string
	RunRef         string
}

func uploadEvidenceConfigFromEnv() uploadEvidenceConfig {
	return uploadEvidenceConfig{
		EvidenceDir:    strings.TrimSpace(os.Getenv("GLIMMUNG_EVIDENCE_DIR")),
		StorageAccount: strings.TrimSpace(os.Getenv("AGENT_SCREENSHOT_STORAGE_ACCOUNT")),
		Container:      strings.TrimSpace(os.Getenv("AGENT_SCREENSHOT_CONTAINER")),
		Project:        strings.TrimSpace(os.Getenv("GLIMMUNG_PROJECT")),
		RunRef:         strings.TrimSpace(os.Getenv("GLIMMUNG_RUN_REF")),
	}
}

// evidencePrefix is the run-scoped blob prefix every uploaded object lands under:
// <project>/<run-ref>. Empty segments are dropped so a missing project/run-ref
// degrades to a flatter prefix rather than producing a leading/double slash.
func (c uploadEvidenceConfig) evidencePrefix() string {
	segments := make([]string, 0, 2)
	for _, segment := range []string{c.Project, c.RunRef} {
		segment = strings.Trim(strings.TrimSpace(segment), "/")
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return strings.Join(segments, "/")
}

// planEvidenceUploads walks evidenceDir and returns the ordered set of
// (blobName, bytes) PUTs: every regular file under the directory, keyed as
// <prefix>/<relpath> with POSIX separators, sorted by blob name for
// determinism. An empty or absent directory yields nil (the caller treats that
// as a success no-op). Symlinks are not followed; only regular files upload.
func planEvidenceUploads(evidenceDir, prefix string) ([]plannedUpload, error) {
	evidenceDir = strings.TrimSpace(evidenceDir)
	if evidenceDir == "" {
		return nil, nil
	}
	info, err := os.Stat(evidenceDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat evidence dir %q: %w", evidenceDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("evidence dir %q is not a directory", evidenceDir)
	}
	var planned []plannedUpload
	walkErr := filepath.WalkDir(evidenceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(evidenceDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read evidence file %q: %w", path, err)
		}
		planned = append(planned, plannedUpload{
			BlobName:    joinBlobPath(prefix, rel),
			Body:        body,
			ContentType: evidenceContentType(rel),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(planned, func(i, j int) bool { return planned[i].BlobName < planned[j].BlobName })
	return planned, nil
}

func joinBlobPath(prefix, rel string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	rel = strings.TrimLeft(strings.TrimSpace(rel), "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

// evidenceContentType infers a content type from the file extension, defaulting
// to application/octet-stream. The artifact-serving layer keys evidence kind off
// the ref/extension too, so a correct image/* or video/* type keeps the report
// render honest.
func evidenceContentType(rel string) string {
	if ct := mime.TypeByExtension(filepath.Ext(rel)); strings.TrimSpace(ct) != "" {
		return ct
	}
	return "application/octet-stream"
}

// runUploadEvidence is the subcommand entrypoint. It reads the environment,
// no-ops on empty/absent evidence, constructs the real Azure-backed client, and
// performs the uploads. Emitted artifact refs are surfaced to the run
// report/UI via GLIMMUNG_COMPLETION_FILE when the parent runner set it (the
// same completion-metadata channel agent/verification steps use).
func runUploadEvidence(ctx context.Context, _ []string) error {
	cfg := uploadEvidenceConfigFromEnv()
	if cfg.EvidenceDir == "" {
		log.Print("no evidence to upload: GLIMMUNG_EVIDENCE_DIR is empty")
		return nil
	}
	planned, err := planEvidenceUploads(cfg.EvidenceDir, cfg.evidencePrefix())
	if err != nil {
		return err
	}
	if len(planned) == 0 {
		log.Printf("no evidence to upload: %s is empty or absent", cfg.EvidenceDir)
		return nil
	}
	if cfg.StorageAccount == "" || cfg.Container == "" {
		return errors.New("AGENT_SCREENSHOT_STORAGE_ACCOUNT and AGENT_SCREENSHOT_CONTAINER are required to upload evidence")
	}
	client, err := newAzureEvidenceBlobClient(cfg.StorageAccount)
	if err != nil {
		return err
	}
	refs, err := uploadPlannedEvidence(ctx, client, cfg.Container, planned)
	if err != nil {
		return err
	}
	log.Printf("uploaded %d evidence artifact(s) to %s/%s under prefix %q", len(refs), cfg.StorageAccount, cfg.Container, cfg.evidencePrefix())
	if err := emitEvidenceCompletion(refs); err != nil {
		// Surfacing refs is best-effort: the upload already succeeded, so a
		// completion-file write failure must not fail the step.
		log.Printf("warning: could not emit evidence refs to completion file: %v", err)
	}
	return nil
}

// uploadPlannedEvidence PUTs each planned object with overwrite semantics
// (UploadBuffer overwrites by default — idempotent, matching the bash
// `--overwrite true`) and returns the blob:// refs the report consumes.
func uploadPlannedEvidence(ctx context.Context, client evidenceBlobClient, container string, planned []plannedUpload) ([]string, error) {
	refs := make([]string, 0, len(planned))
	for _, item := range planned {
		if err := client.UploadBuffer(ctx, container, item.BlobName, item.Body, item.ContentType); err != nil {
			return nil, fmt.Errorf("upload evidence blob %q: %w", item.BlobName, err)
		}
		refs = append(refs, "blob://"+container+"/"+item.BlobName)
	}
	return refs, nil
}

// emitEvidenceCompletion writes the uploaded artifact refs into the step's
// GLIMMUNG_COMPLETION_FILE as evidence artifacts. The parent runner reads that
// file after the step and folds the evidence into the job's /completed
// callback, so the refs surface in the run report's evidence_refs /
// screenshots_markdown channels. When the file is not set (the subcommand was
// invoked outside a managed step) this is a no-op.
func emitEvidenceCompletion(refs []string) error {
	completionFile := strings.TrimSpace(os.Getenv("GLIMMUNG_COMPLETION_FILE"))
	if completionFile == "" || len(refs) == 0 {
		return nil
	}
	artifacts := make([]evidenceArtifact, 0, len(refs))
	var md strings.Builder
	md.WriteString("### Uploaded evidence\n\n")
	for _, ref := range refs {
		label := ref[strings.LastIndex(ref, "/")+1:]
		artifacts = append(artifacts, evidenceArtifact{
			Kind:  evidenceKindForRef(ref),
			Ref:   ref,
			Label: label,
		})
		fmt.Fprintf(&md, "- %s (`%s`)\n", label, ref)
	}
	markdown := md.String()
	payload := completionMetadata{
		Evidence:            artifacts,
		ScreenshotsMarkdown: markdown,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return os.WriteFile(completionFile, body, 0o600)
}

// evidenceKindForRef classifies a ref by extension into the evidence kinds the
// report understands. Unknown extensions fall back to "artifact".
func evidenceKindForRef(ref string) string {
	switch strings.ToLower(filepath.Ext(ref)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return "screenshot"
	case ".webm", ".mp4", ".mov":
		return "video"
	default:
		return "artifact"
	}
}

// azureEvidenceBlobClient adapts the real azblob client to evidenceBlobClient.
type azureEvidenceBlobClient struct {
	client *azblob.Client
}

func newAzureEvidenceBlobClient(storageAccount string) (*azureEvidenceBlobClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", strings.TrimSpace(storageAccount))
	client, err := azblob.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create blob client: %w", err)
	}
	return &azureEvidenceBlobClient{client: client}, nil
}

func (c *azureEvidenceBlobClient) UploadBuffer(ctx context.Context, container, blobName string, body []byte, contentType string) error {
	ct := contentType
	if strings.TrimSpace(ct) == "" {
		ct = "application/octet-stream"
	}
	_, err := c.client.UploadBuffer(ctx, container, blobName, body, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &ct},
	})
	return err
}
