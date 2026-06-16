package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DeploySlotToImage redeploys a test slot's hot runtime to a specific,
// already-CI-built image for a verified commit. It is the replacement for the
// artifact build-and-stream hot-swap (apply_test_slot_hot_swap): instead of
// building artifacts in an ephemeral Job and streaming them into running pods,
// it re-runs the slot's own helm reconcile at the verified ref with the image
// overridden to the CI-built tag, then verifies the slot is actually running
// that image. The slot ends up running exactly what CI built and main will
// ship — correct by construction, no rebuild, no streamed artifact.
//
// Inputs (the caller owns the legitimacy gate and resolution):
//   - verifiedRef: the published, gate-passed commit (published + CI-green +
//     mergeable + current-with-main). Rendered as the chart ref so the branch's
//     own chart/template changes are exercised too.
//   - image: the CI-built image tag/ref for that exact commit (e.g. a git-SHA
//     tag). Resolution is the caller's job.
//   - imageValueKey: the chart's image value path the override sets
//     (helm --set <imageValueKey>=<image>). Empty means "no override" — the
//     chart at verifiedRef already pins the image.
func (l *KubernetesRunLauncher) DeploySlotToImage(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter, verifiedRef, image, imageValueKey string) error {
	slotName := strings.TrimSpace(mapStringValueOrEmpty(lease.Metadata, "runner_slot_name"))
	if slotName == "" {
		return fmt.Errorf("lease has no runner_slot_name")
	}
	verifiedRef = strings.TrimSpace(verifiedRef)
	if verifiedRef == "" {
		return fmt.Errorf("verified git ref is required")
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("resolved image is required")
	}
	config, ok := testSlotHelmConfig(project)
	if !ok {
		return fmt.Errorf("project %s has no enabled test_slot_helm config", project.Name)
	}
	if strings.TrimSpace(project.GitHubRepo) == "" {
		return fmt.Errorf("github_repo is required to deploy a slot")
	}
	if minter == nil {
		return fmt.Errorf("github token minter is required to deploy a slot")
	}

	// Deploy the verified commit's chart with its CI-built image pinned in. We
	// render at the verified ref so chart/template changes on the branch are
	// exercised, and override the image to the CI build for that exact commit
	// so the slot runs precisely what shipped. This reuses the same reconcile
	// path as activation (runTestSlotHelmReconcile, renderMode=hot) — the
	// tested deploy mechanism, not a bespoke patch.
	config.GitRef = verifiedRef
	if key := strings.TrimSpace(imageValueKey); key != "" {
		if config.Values == nil {
			config.Values = map[string]string{}
		}
		config.Values[key] = image
	}
	if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeHot); err != nil {
		return fmt.Errorf("deploy slot %s to %s: %w", slotName, image, err)
	}

	// Verify the slot is actually running the image we deployed — observed from
	// the apiserver, not assumed because the reconcile returned. This is the
	// "reached the right destination" gate: a reconcile that silently kept a
	// stale image (helm no-op, wrong value key, image pull never rolled) fails
	// here loudly instead of presenting a stale slot as validated.
	if err := l.verifyTestSlotRunningImage(ctx, slotName, image); err != nil {
		return fmt.Errorf("deploy slot %s verify: %w", slotName, err)
	}
	return nil
}

// verifyTestSlotRunningImage confirms at least one container in the slot's app
// namespace is running wantImage. It reads live pod specs from the apiserver and
// asserts against the *running* pods (not the Deployment spec), which catches
// the case where the Deployment was updated but a new pod never rolled. wantImage
// may be a full ref (registry/repo:tag) or just a tag; a pod image matches when
// it equals or ends with wantImage, so both a full-image and a tag-only override
// verify correctly.
func (l *KubernetesRunLauncher) verifyTestSlotRunningImage(ctx context.Context, namespace, wantImage string) error {
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
	status, list, err := l.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	if status >= 400 {
		return fmt.Errorf("list pods in %s returned %d", namespace, status)
	}
	wantImage = strings.TrimSpace(wantImage)
	items, _ := list["items"].([]any)
	seen := map[string]struct{}{}
	for _, raw := range items {
		pod, _ := raw.(map[string]any)
		spec, _ := pod["spec"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		for _, c := range containers {
			container, _ := c.(map[string]any)
			img, _ := container["image"].(string)
			img = strings.TrimSpace(img)
			if img == "" {
				continue
			}
			if img == wantImage || strings.HasSuffix(img, wantImage) {
				return nil
			}
			seen[img] = struct{}{}
		}
	}
	observed := make([]string, 0, len(seen))
	for img := range seen {
		observed = append(observed, img)
	}
	return fmt.Errorf("no running container in %s is on image %q (observed: %s)", namespace, wantImage, strings.Join(observed, ", "))
}
