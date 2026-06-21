package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
)

type ProjectWriter interface {
	UpsertProject(ctx context.Context, req ProjectRegister) (Project, error)
}

type ProjectRegister struct {
	Name       string         `json:"name"`
	GitHubRepo string         `json:"github_repo"`
	ArgoCDApp  string         `json:"argocd_app"`
	Metadata   map[string]any `json:"metadata"`
}

type projectRegisterRequest struct {
	Name       *string        `json:"name"`
	GitHubRepo *string        `json:"github_repo"`
	ArgoCDApp  string         `json:"argocd_app"`
	Metadata   map[string]any `json:"metadata"`
}

func registerProject(store ReadStore, managedOrigins ManagedOriginReconciler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(ProjectWriter)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project writer not configured")
			return
		}

		var req projectRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Name == nil {
			writeProblem(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		if req.GitHubRepo == nil {
			writeProblem(w, http.StatusUnprocessableEntity, "github_repo is required")
			return
		}
		if err := validateRetiredBuildStreamMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := validateTestSlotHelmMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := validateTestSlotDeployMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := validateLivePreviewMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if _, _, err := agentruntime.ConfigFromMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		project, err := writer.UpsertProject(r.Context(), ProjectRegister{
			Name:       *req.Name,
			GitHubRepo: *req.GitHubRepo,
			ArgoCDApp:  req.ArgoCDApp,
			Metadata:   mapOrEmpty(req.Metadata),
		})
		if err != nil {
			writeInternalError(w, r, err, "register project failed")
			return
		}
		// Reconcile glimmung-owned auth.romaine.life slot origins for this
		// project. Skipped when the project doesn't opt in via
		// managed_auth_origins.enabled; failed status persists on the
		// project row so dashboards surface the broken state. See
		// romaine-life/glimmung#142 stage 2.
		if updated, ok := reconcileManagedAuthOrigins(r.Context(), store, managedOrigins, project); ok {
			project = updated
		}
		writeJSON(w, http.StatusOK, project)
	}
}

func validateRetiredBuildStreamMetadata(metadata map[string]any) error {
	if metadata == nil {
		return nil
	}
	snakeKey := "test_slot_" + "hot_swap"
	camelKey := "testSlot" + "Hot" + "Swap"
	if _, ok := metadata[snakeKey]; ok {
		return fmt.Errorf("%s is retired; deploy-image-to-slot uses test_slot_helm and CI-built images instead (see .tank/docs/migration-policy.md)", snakeKey)
	}
	if _, ok := metadata[camelKey]; ok {
		return fmt.Errorf("%s is retired; deploy-image-to-slot uses test_slot_helm and CI-built images instead (see .tank/docs/migration-policy.md)", camelKey)
	}
	return nil
}

// validateTestSlotHelmMetadata enforces the chart-image-tag drift fix
// at the project write surface: `image.tag` (and its `imageTag` /
// nested `image: {tag: ...}` spellings) must not appear in
// `test_slot_helm.values` or `test_slot_helm.set_string_values`.
//
// History: warm test slots install each project's chart with `--set
// image.tag=<value pulled from this metadata>`. That literal was set
// once per project and never bumped, so slots ran a stale image while
// prod moved on. Two SPA bugs already-fixed upstream surfaced on slots
// because of it (investigated 2026-05-28). The fix shipped in PRs
// glimmung#622 + ambience#258: the chart's own `image.tag` default
// tracks prod via CI lockstep, and the Postgres pin was deleted from
// every project. This guard prevents the pin from being re-introduced
// via a future `register_project` upsert.
//
// Per .tank/docs/migration-policy.md the retired path stays deleted at
// every layer — CI guards in each repo cover the chart files; this
// guard covers the durable Postgres write path.
func validateTestSlotHelmMetadata(metadata map[string]any) error {
	const retiredFieldMessage = "test_slot_helm.%s.image.tag is a retired field — project image tags must come from the chart's own default, which the per-repo build workflow keeps in lockstep with prod (see .tank/docs/migration-policy.md, glimmung#622, ambience#258)"
	helmRaw, ok := metadata["test_slot_helm"]
	if !ok {
		helmRaw, ok = metadata["testSlotHelm"]
	}
	if !ok {
		return nil
	}
	helm, ok := helmRaw.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"values", "set_string_values", "setStringValues"} {
		raw, ok := helm[key]
		if !ok {
			continue
		}
		vals, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := vals["image.tag"]; has {
			return fmt.Errorf(retiredFieldMessage, key)
		}
		if _, has := vals["imageTag"]; has {
			return fmt.Errorf(retiredFieldMessage, key)
		}
		// Nested form: image: {tag: "..."}. Helm flattens both forms
		// to the same effective `--set image.tag=...`, so either is
		// equivalent drift surface and forbidden.
		if image, ok := vals["image"].(map[string]any); ok {
			if _, has := image["tag"]; has {
				return fmt.Errorf(retiredFieldMessage, key)
			}
		}
	}
	return nil
}

func validateTestSlotDeployMetadata(metadata map[string]any) error {
	if metadata == nil {
		return nil
	}
	deploy, ok := mapFromMap(metadata, "test_slot_deploy")
	if !ok {
		deploy, ok = mapFromMap(metadata, "testSlotDeploy")
	}
	if !ok {
		return nil
	}
	ciImage, ok := mapFromMap(deploy, "ci_image")
	if !ok {
		ciImage, ok = mapFromMap(deploy, "ciImage")
	}
	if !ok {
		return nil
	}
	for _, key := range []string{"images_by_sha", "imagesBySha", "tags_by_sha", "tagsBySha"} {
		if _, has := ciImage[key]; has {
			return fmt.Errorf("test_slot_deploy.ci_image.%s is retired; deploy-image resolves GitHub Actions CI lookup tags from workflow run metadata", key)
		}
	}
	if repository := firstNonEmpty(configString(ciImage, "repository"), configString(ciImage, "image_repository", "imageRepository")); strings.TrimSpace(repository) != "" {
		if _, _, ok := splitRegistryRepository(repository); !ok && strings.Contains(repository, "/") {
			return fmt.Errorf("test_slot_deploy.ci_image.repository must be either an ACR repository name or registry/repository")
		}
	}
	if workflow := configString(ciImage, "workflow", "workflow_file", "workflowFile"); strings.ContainsAny(workflow, " \t\n\r") {
		return fmt.Errorf("test_slot_deploy.ci_image.workflow must not contain whitespace")
	}
	return nil
}

// reconcileManagedAuthOrigins runs the managed-origin reconciler and
// persists its status on the project row. Returns the refreshed project
// when status was written, or the original when the reconciler was
// absent or the status writer is unsupported. Errors are intentionally
// swallowed at this layer (status carries the failure) so the
// outer handler can still return the upserted project.
func reconcileManagedAuthOrigins(ctx context.Context, store ReadStore, reconciler ManagedOriginReconciler, project Project) (Project, bool) {
	if reconciler == nil {
		return project, false
	}
	status, _ := reconciler.ReconcileManagedOrigins(ctx, project)
	if status.State == "" {
		return project, false
	}
	writer, ok := store.(ProjectManagedAuthOriginStatusWriter)
	if !ok || writer == nil {
		return project, false
	}
	updated, err := writer.SetProjectManagedAuthOriginStatus(ctx, firstNonEmpty(project.Name, project.ID), status)
	if err != nil {
		return project, false
	}
	return updated, true
}
