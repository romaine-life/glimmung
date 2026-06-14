package runnermcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"strings"
)

// ToolUploadEvidence is the stable name of the upload_evidence tool. It is the
// single source of truth shared by the tool registration, the registration-time
// catalog, and any caller that needs to reference the tool by name.
const ToolUploadEvidence = "upload_evidence"

const uploadEvidenceSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path to the evidence file, relative to the run workspace. No '..'."},
    "kind": {"type": "string", "enum": ["video", "screenshot"], "description": "Evidence kind. Inferred from the file extension when omitted."},
    "label": {"type": "string", "description": "Optional human-readable label."},
    "content_type": {"type": "string", "description": "Optional MIME type; inferred from the file extension when omitted."}
  },
  "required": ["path"]
}`

// NewUploadEvidenceTool builds the upload_evidence tool: it reads an evidence
// file the agent produced in the run workspace and uploads it to Glimmung's
// artifact store under the run's canonical prefix
// (runs/<project>/<run-id>/<videos|screenshots>/<file>), returning a durable
// reference the agent can cite as evidence.
//
// It replaces the per-repo, hand-rolled `az storage blob upload-batch` scripts
// the audit found duplicated across projects with one structured, run-scoped
// path: the agent cannot pick the storage account, the bucket, or the layout —
// those are Glimmung's, fixed here.
func NewUploadEvidenceTool(rc RunContext, workspace fs.FS, up ArtifactUploader) Tool {
	return Tool{
		Name:        ToolUploadEvidence,
		Description: "Upload an evidence file from the run workspace to Glimmung artifact storage and return its durable reference.",
		InputSchema: json.RawMessage(uploadEvidenceSchema),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Path        string `json:"path"`
				Kind        string `json:"kind"`
				Label       string `json:"label"`
				ContentType string `json:"content_type"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("upload_evidence: invalid arguments: %w", err)
			}

			rel := strings.TrimPrefix(strings.TrimSpace(args.Path), "./")
			if rel == "" {
				return nil, fmt.Errorf("upload_evidence: path is required")
			}
			if !fs.ValidPath(rel) {
				return nil, fmt.Errorf("upload_evidence: path %q must be workspace-relative with no '..'", args.Path)
			}
			if strings.TrimSpace(rc.Project) == "" || strings.TrimSpace(rc.RunID) == "" {
				return nil, fmt.Errorf("upload_evidence: run context is missing project/run id")
			}

			kindDir, kind, err := evidenceKindDir(args.Kind, rel)
			if err != nil {
				return nil, err
			}

			body, err := fs.ReadFile(workspace, rel)
			if err != nil {
				return nil, fmt.Errorf("upload_evidence: read %q: %w", rel, err)
			}
			if len(body) == 0 {
				return nil, fmt.Errorf("upload_evidence: %q is empty", rel)
			}

			contentType := strings.TrimSpace(args.ContentType)
			if contentType == "" {
				contentType = contentTypeForExt(path.Ext(rel))
			}

			blobName := path.Join("runs", rc.Project, rc.RunID, kindDir, path.Base(rel))
			size, err := up.Upload(ctx, blobName, body, contentType)
			if err != nil {
				return nil, fmt.Errorf("upload_evidence: upload %q: %w", blobName, err)
			}

			return map[string]any{
				"ref":          "blob://artifacts/" + blobName,
				"url":          artifactURL(blobName),
				"blob_name":    blobName,
				"kind":         kind,
				"label":        strings.TrimSpace(args.Label),
				"content_type": contentType,
				"size_bytes":   size,
			}, nil
		},
	}
}

// evidenceKindDir resolves the storage subdirectory and the normalized kind. An
// explicit kind wins; otherwise it is inferred from the file extension.
func evidenceKindDir(kind, rel string) (dir, normalized string, err error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "video":
		return "videos", "video", nil
	case "screenshot", "image":
		return "screenshots", "screenshot", nil
	case "":
		switch contentCategory(path.Ext(rel)) {
		case "video":
			return "videos", "video", nil
		case "image":
			return "screenshots", "screenshot", nil
		}
		return "", "", fmt.Errorf("upload_evidence: kind is required and could not be inferred from %q", rel)
	default:
		return "", "", fmt.Errorf("upload_evidence: unsupported kind %q (want video or screenshot)", kind)
	}
}

func contentCategory(ext string) string {
	switch strings.ToLower(ext) {
	case ".webm", ".mp4", ".mov", ".m4v":
		return "video"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return "image"
	}
	return ""
}

func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".webm":
		return "video/webm"
	case ".mp4":
		return "video/mp4"
	case ".mov", ".m4v":
		return "video/quicktime"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// artifactURL builds the per-segment-escaped serving URL for a blob, matching
// the server's existing /v1/artifacts/<escaped-path> convention.
func artifactURL(blobName string) string {
	parts := strings.Split(strings.Trim(blobName, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/v1/artifacts/" + strings.Join(parts, "/")
}
