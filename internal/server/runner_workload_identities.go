package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// maxFederatedIdentityCredentialsPerIdentity bounds how many federated identity
// credentials a single Azure user-assigned managed identity may carry — Azure's
// documented limit. The standby pool already consumes some; each live preview
// consumes one more per credential template. The provision refuses (with a
// surfaced durable error) rather than letting Azure reject an over-cap upsert
// with an opaque 400, so a capacity ceiling is an explicit, observable failure.
const maxFederatedIdentityCredentialsPerIdentity = 20

const (
	RunnerWorkloadIdentityStatusOK      = "ok"
	RunnerWorkloadIdentityStatusSkipped = "skipped"
	RunnerWorkloadIdentityStatusFailed  = "failed"

	defaultWorkloadIdentityAudience = "api://AzureADTokenExchange"
)

type FederatedIdentityCredentialRef struct {
	SubscriptionID string
	ResourceGroup  string
	IdentityName   string
	CredentialName string
}

type FederatedIdentityCredential struct {
	FederatedIdentityCredentialRef
	Issuer    string
	Subject   string
	Audiences []string
}

type FederatedIdentityCredentialClient interface {
	UpsertFederatedIdentityCredential(ctx context.Context, credential FederatedIdentityCredential) error
	ListFederatedIdentityCredentials(ctx context.Context, ref FederatedIdentityCredentialRef) ([]FederatedIdentityCredential, error)
	DeleteFederatedIdentityCredential(ctx context.Context, ref FederatedIdentityCredentialRef) error
}

type RunnerWorkloadIdentityReconciler interface {
	ReconcileRunnerWorkloadIdentities(ctx context.Context, project Project) (RunnerWorkloadIdentityStatus, error)
}

type ProjectRunnerWorkloadIdentityStatusWriter interface {
	SetProjectRunnerWorkloadIdentityStatus(ctx context.Context, project string, status RunnerWorkloadIdentityStatus) (Project, error)
}

type RunnerWorkloadIdentityStatus struct {
	State              string                                   `json:"state"`
	Provider           string                                   `json:"provider,omitempty"`
	SubscriptionID     string                                   `json:"subscription_id,omitempty"`
	ResourceGroup      string                                   `json:"resource_group,omitempty"`
	Issuer             string                                   `json:"issuer,omitempty"`
	DesiredCount       int                                      `json:"desired_count"`
	ManagedCredentials []RunnerWorkloadIdentityCredentialStatus `json:"managed_credentials"`
	Upserted           []RunnerWorkloadIdentityCredentialStatus `json:"upserted,omitempty"`
	Deleted            []RunnerWorkloadIdentityCredentialStatus `json:"deleted,omitempty"`
	LastReconciledAt   string                                   `json:"last_reconciled_at,omitempty"`
	LastError          *string                                  `json:"last_error,omitempty"`
}

type RunnerWorkloadIdentityCredentialStatus struct {
	IdentityName   string   `json:"identity_name"`
	CredentialName string   `json:"credential_name"`
	Subject        string   `json:"subject"`
	Audiences      []string `json:"audiences,omitempty"`
}

type RunnerWorkloadIdentityService struct {
	Client                  FederatedIdentityCredentialClient
	Issuer                  string
	ServiceAccountTokenPath string
	Now                     func() time.Time
}

type runnerWorkloadIdentityConfig struct {
	Enabled        bool
	Provider       string
	SubscriptionID string
	ResourceGroup  string
	Issuer         string
	SlotPrefix     string
	Count          int
	Credentials    []runnerWorkloadIdentityCredentialTemplate
}

type runnerWorkloadIdentityCredentialTemplate struct {
	IdentityName   string
	CredentialName string
	Subject        string
	Audiences      []string
}

func (s RunnerWorkloadIdentityService) ReconcileRunnerWorkloadIdentities(ctx context.Context, project Project) (RunnerWorkloadIdentityStatus, error) {
	cfg, ok, err := runnerWorkloadIdentityConfigFromProject(project)
	if !ok {
		return RunnerWorkloadIdentityStatus{}, nil
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	status := RunnerWorkloadIdentityStatus{
		State:            RunnerWorkloadIdentityStatusFailed,
		Provider:         cfg.Provider,
		SubscriptionID:   cfg.SubscriptionID,
		ResourceGroup:    cfg.ResourceGroup,
		DesiredCount:     cfg.Count,
		LastReconciledAt: now,
	}
	if err == nil {
		cfg.Issuer = firstNonEmpty(cfg.Issuer, strings.TrimSpace(s.Issuer), issuerFromServiceAccountToken(s.ServiceAccountTokenPath))
		status.Issuer = cfg.Issuer
		status.ManagedCredentials = credentialStatusList(desiredWorkloadIdentityCredentials(cfg))
	}
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	if cfg.Issuer == "" {
		err := errors.New("runner_standby_workload_identity requires issuer or RUNNER_WORKLOAD_IDENTITY_ISSUER")
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	if s.Client == nil {
		err := errors.New("runner workload identity client not configured")
		status.LastError = stringPtr(err.Error())
		return status, err
	}

	desired := desiredWorkloadIdentityCredentials(cfg)
	currentByIdentity, err := s.currentCredentialsByIdentity(ctx, cfg)
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	deleted, err := s.deleteRemovedManagedCredentials(ctx, cfg, desired, currentByIdentity)
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	status.Deleted = credentialStatusList(deleted)

	currentSet := workloadIdentityCredentialFullSet(flattenWorkloadIdentityCredentials(currentByIdentity))
	for _, credential := range desired {
		if currentSet[workloadIdentityCredentialFullKey(credential)] {
			continue
		}
		if err := s.Client.UpsertFederatedIdentityCredential(ctx, credential); err != nil {
			err = fmt.Errorf("upsert federated identity credential %s/%s: %w", credential.IdentityName, credential.CredentialName, err)
			status.LastError = stringPtr(err.Error())
			return status, err
		}
		status.Upserted = append(status.Upserted, credentialStatus(credential))
	}

	status.State = RunnerWorkloadIdentityStatusOK
	status.LastError = nil
	return status, nil
}

func (s RunnerWorkloadIdentityService) currentCredentialsByIdentity(ctx context.Context, cfg runnerWorkloadIdentityConfig) (map[string][]FederatedIdentityCredential, error) {
	currentByIdentity := map[string][]FederatedIdentityCredential{}
	seenIdentity := map[string]bool{}
	for _, template := range cfg.Credentials {
		if seenIdentity[template.IdentityName] {
			continue
		}
		seenIdentity[template.IdentityName] = true
		ref := FederatedIdentityCredentialRef{
			SubscriptionID: cfg.SubscriptionID,
			ResourceGroup:  cfg.ResourceGroup,
			IdentityName:   template.IdentityName,
		}
		current, err := s.Client.ListFederatedIdentityCredentials(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("list federated identity credentials for %s: %w", template.IdentityName, err)
		}
		currentByIdentity[template.IdentityName] = current
	}
	return currentByIdentity, nil
}

func (s RunnerWorkloadIdentityService) deleteRemovedManagedCredentials(ctx context.Context, cfg runnerWorkloadIdentityConfig, desired []FederatedIdentityCredential, currentByIdentity map[string][]FederatedIdentityCredential) ([]FederatedIdentityCredential, error) {
	deleted := []FederatedIdentityCredential{}
	desiredSet := workloadIdentityCredentialSet(desired)
	seenIdentity := map[string]bool{}
	for _, template := range cfg.Credentials {
		if seenIdentity[template.IdentityName] {
			continue
		}
		seenIdentity[template.IdentityName] = true
		current := currentByIdentity[template.IdentityName]
		for _, credential := range current {
			if desiredSet[workloadIdentityCredentialKey(credential)] {
				continue
			}
			if _, ok := managedWorkloadIdentityCredentialSlotName(credential, cfg); !ok {
				continue
			}
			if err := s.Client.DeleteFederatedIdentityCredential(ctx, credential.FederatedIdentityCredentialRef); err != nil {
				return nil, fmt.Errorf("delete federated identity credential %s/%s: %w", credential.IdentityName, credential.CredentialName, err)
			}
			deleted = append(deleted, credential)
		}
	}
	return deleted, nil
}

func (s RunnerWorkloadIdentityService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func runnerWorkloadIdentityConfigFromProject(project Project) (runnerWorkloadIdentityConfig, bool, error) {
	cfgMap, ok := mapFromMap(project.Metadata, "runner_standby_workload_identity")
	if !ok {
		cfgMap, ok = mapFromMap(project.Metadata, "runnerStandbyWorkloadIdentity")
	}
	if !ok || !boolFromMap(cfgMap, "enabled") {
		return runnerWorkloadIdentityConfig{}, false, nil
	}
	standby, standbyOK := mapFromMap(project.Metadata, "runner_standby_dns")
	if !standbyOK {
		standby, standbyOK = mapFromMap(project.Metadata, "runnerStandbyDns")
	}
	cfg := runnerWorkloadIdentityConfig{
		Enabled:        true,
		Provider:       firstNonEmpty(stringMapValue(cfgMap, "provider"), "azure"),
		SubscriptionID: firstNonEmpty(stringMapValue(cfgMap, "subscription"), stringMapValue(cfgMap, "subscription_id"), stringMapValue(cfgMap, "subscriptionId")),
		ResourceGroup:  firstNonEmpty(stringMapValue(cfgMap, "resource_group"), stringMapValue(cfgMap, "resourceGroup")),
		Issuer:         firstNonEmpty(stringMapValue(cfgMap, "issuer"), stringMapValue(cfgMap, "issuer_url"), stringMapValue(cfgMap, "issuerUrl")),
		SlotPrefix:     firstNonEmpty(stringMapValue(standby, "slot_prefix"), stringMapValue(standby, "slotPrefix")),
		Count:          nonNegativeIntMapValue(cfgMap, "count"),
	}
	if cfg.Count == 0 {
		cfg.Count = nonNegativeIntMapValue(standby, "count")
	}
	switch cfg.Provider {
	case "", "azure":
		cfg.Provider = "azure"
	default:
		return cfg, true, fmt.Errorf("unsupported runner workload identity provider %q", cfg.Provider)
	}
	if cfg.SubscriptionID == "" {
		return cfg, true, errors.New("runner_standby_workload_identity.subscription is required")
	}
	if cfg.ResourceGroup == "" {
		return cfg, true, errors.New("runner_standby_workload_identity.resource_group is required")
	}
	if !standbyOK {
		return cfg, true, errors.New("runner_standby_dns metadata is required")
	}
	if cfg.SlotPrefix == "" {
		return cfg, true, errors.New("runner_standby_dns.slot_prefix is required")
	}
	credentials := workloadIdentityCredentialTemplatesFromMap(cfgMap)
	if len(credentials) == 0 {
		return cfg, true, errors.New("runner_standby_workload_identity.credentials is required")
	}
	cfg.Credentials = credentials
	return cfg, true, nil
}

func workloadIdentityCredentialTemplatesFromMap(values map[string]any) []runnerWorkloadIdentityCredentialTemplate {
	rows := anySlice(firstAny(values["credentials"], values["federated_credentials"], values["federatedCredentials"]))
	templates := make([]runnerWorkloadIdentityCredentialTemplate, 0, len(rows))
	for _, row := range rows {
		mapped := anyMap(row)
		template := runnerWorkloadIdentityCredentialTemplate{
			IdentityName:   firstNonEmpty(stringMapValue(mapped, "identity_name"), stringMapValue(mapped, "identityName")),
			CredentialName: firstNonEmpty(stringMapValue(mapped, "credential_name"), stringMapValue(mapped, "credentialName"), stringMapValue(mapped, "name")),
			Subject:        stringMapValue(mapped, "subject"),
			Audiences:      stringSliceFromMap(mapped, "audiences", "audience"),
		}
		if len(template.Audiences) == 0 {
			template.Audiences = []string{defaultWorkloadIdentityAudience}
		}
		if template.IdentityName == "" || template.CredentialName == "" || template.Subject == "" {
			continue
		}
		templates = append(templates, template)
	}
	return templates
}

func desiredWorkloadIdentityCredentials(cfg runnerWorkloadIdentityConfig) []FederatedIdentityCredential {
	credentials := make([]FederatedIdentityCredential, 0, cfg.Count*len(cfg.Credentials))
	for slotIndex := 1; slotIndex <= cfg.Count; slotIndex++ {
		slotName := fmt.Sprintf("%s-%d", cfg.SlotPrefix, slotIndex)
		substitutions := workloadIdentitySubstitutions(cfg, slotIndex, slotName)
		for _, template := range cfg.Credentials {
			credentials = append(credentials, FederatedIdentityCredential{
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: cfg.SubscriptionID,
					ResourceGroup:  cfg.ResourceGroup,
					IdentityName:   template.IdentityName,
					CredentialName: formatSubstitutions(template.CredentialName, substitutions),
				},
				Issuer:    cfg.Issuer,
				Subject:   formatSubstitutions(template.Subject, substitutions),
				Audiences: append([]string{}, template.Audiences...),
			})
		}
	}
	sort.SliceStable(credentials, func(i, j int) bool {
		left, right := credentials[i], credentials[j]
		if left.IdentityName != right.IdentityName {
			return left.IdentityName < right.IdentityName
		}
		return left.CredentialName < right.CredentialName
	})
	return credentials
}

func workloadIdentityCredentialSet(credentials []FederatedIdentityCredential) map[string]bool {
	out := map[string]bool{}
	for _, credential := range credentials {
		out[workloadIdentityCredentialKey(credential)] = true
	}
	return out
}

func workloadIdentityCredentialKey(credential FederatedIdentityCredential) string {
	return credential.IdentityName + "\x00" + credential.CredentialName + "\x00" + credential.Subject
}

func workloadIdentityCredentialFullSet(credentials []FederatedIdentityCredential) map[string]bool {
	out := map[string]bool{}
	for _, credential := range credentials {
		out[workloadIdentityCredentialFullKey(credential)] = true
	}
	return out
}

func workloadIdentityCredentialFullKey(credential FederatedIdentityCredential) string {
	audiences := append([]string{}, credential.Audiences...)
	sort.Strings(audiences)
	return workloadIdentityCredentialKey(credential) + "\x00" + credential.Issuer + "\x00" + strings.Join(audiences, "\x00")
}

func flattenWorkloadIdentityCredentials(currentByIdentity map[string][]FederatedIdentityCredential) []FederatedIdentityCredential {
	var out []FederatedIdentityCredential
	for _, credentials := range currentByIdentity {
		out = append(out, credentials...)
	}
	return out
}

func workloadIdentitySubstitutions(cfg runnerWorkloadIdentityConfig, slotIndex int, slotName string) map[string]string {
	return map[string]string{
		"project":    cfg.SlotPrefix,
		"slot_index": strconv.Itoa(slotIndex),
		"slot_name":  slotName,
		"namespace":  slotName,
	}
}

func managedWorkloadIdentityCredentialIndex(credential FederatedIdentityCredential, cfg runnerWorkloadIdentityConfig) (int, bool) {
	slotName, ok := managedWorkloadIdentityCredentialSlotName(credential, cfg)
	if !ok {
		return 0, false
	}
	index := workloadIdentitySlotIndex(slotName)
	return index, index > 0
}

func managedWorkloadIdentityCredentialSlotName(credential FederatedIdentityCredential, cfg runnerWorkloadIdentityConfig) (string, bool) {
	for _, template := range cfg.Credentials {
		if credential.IdentityName != template.IdentityName {
			continue
		}
		for _, slotName := range workloadIdentitySlotNameCandidates(credential, template) {
			index := workloadIdentitySlotIndex(slotName)
			if index < 1 {
				continue
			}
			substitutions := workloadIdentitySubstitutions(cfg, index, slotName)
			if credential.CredentialName != formatSubstitutions(template.CredentialName, substitutions) {
				continue
			}
			if credential.Subject != formatSubstitutions(template.Subject, substitutions) {
				continue
			}
			return slotName, true
		}
	}
	return "", false
}

func workloadIdentitySlotNameCandidates(credential FederatedIdentityCredential, template runnerWorkloadIdentityCredentialTemplate) []string {
	seen := map[string]bool{}
	var candidates []string
	for _, candidate := range slotNameCandidatesFromTemplate(credential.CredentialName, template.CredentialName) {
		if !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range slotNameCandidatesFromTemplate(credential.Subject, template.Subject) {
		if !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	sort.Strings(candidates)
	return candidates
}

func slotNameCandidatesFromTemplate(value, template string) []string {
	if !strings.Contains(template, "{slot_name}") && !strings.Contains(template, "{namespace}") {
		return nil
	}
	pattern := regexp.QuoteMeta(template)
	slotPattern := `([a-z0-9](?:[a-z0-9-]*[a-z0-9])?-[1-9][0-9]*)`
	pattern = strings.ReplaceAll(pattern, `\{slot_name\}`, slotPattern)
	pattern = strings.ReplaceAll(pattern, `\{namespace\}`, slotPattern)
	pattern = strings.ReplaceAll(pattern, `\{slot_index\}`, `[1-9][0-9]*`)
	pattern = "^" + pattern + "$"
	matches := regexp.MustCompile(pattern).FindStringSubmatch(value)
	if len(matches) <= 1 {
		return nil
	}
	seen := map[string]bool{}
	var candidates []string
	for _, match := range matches[1:] {
		if match == "" || seen[match] {
			continue
		}
		seen[match] = true
		candidates = append(candidates, match)
	}
	return candidates
}

func workloadIdentitySlotIndex(slotName string) int {
	idx := strings.LastIndex(slotName, "-")
	if idx < 0 || idx == len(slotName)-1 {
		return 0
	}
	index, err := strconv.Atoi(slotName[idx+1:])
	if err != nil || index < 1 {
		return 0
	}
	return index
}

func credentialStatusList(credentials []FederatedIdentityCredential) []RunnerWorkloadIdentityCredentialStatus {
	statuses := make([]RunnerWorkloadIdentityCredentialStatus, 0, len(credentials))
	for _, credential := range credentials {
		statuses = append(statuses, credentialStatus(credential))
	}
	return statuses
}

func credentialStatus(credential FederatedIdentityCredential) RunnerWorkloadIdentityCredentialStatus {
	return RunnerWorkloadIdentityCredentialStatus{
		IdentityName:   credential.IdentityName,
		CredentialName: credential.CredentialName,
		Subject:        credential.Subject,
		Audiences:      append([]string{}, credential.Audiences...),
	}
}

func issuerFromServiceAccountToken(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Issuer)
}

// previewWorkloadIdentitySubstitutions binds a preview environment's name to the
// {namespace}/{slot_name} placeholders the project's credential templates use. A
// preview's namespace IS its name (see preview_provision.go), so both resolve to
// previewName; {slot_index} has no preview analogue (the in-scope app templates
// use only {slot_name}/{namespace}).
func previewWorkloadIdentitySubstitutions(cfg runnerWorkloadIdentityConfig, previewName string) map[string]string {
	return map[string]string{
		"project":    cfg.SlotPrefix,
		"slot_index": "preview",
		"slot_name":  previewName,
		"namespace":  previewName,
	}
}

// previewWorkloadIdentityCredentials are the federated identity credentials a
// preview environment needs — one per project credential template, templated for
// the preview's own namespace. It mirrors desiredWorkloadIdentityCredentials but
// for a single ad-hoc preview namespace instead of the fixed standby pool, so an
// app whose stable backend authenticates to Azure via workload identity can boot
// in a preview: without a matching federated credential the backend gets
// AADSTS700213 and never becomes ready.
func previewWorkloadIdentityCredentials(cfg runnerWorkloadIdentityConfig, previewName string) []FederatedIdentityCredential {
	subs := previewWorkloadIdentitySubstitutions(cfg, previewName)
	creds := make([]FederatedIdentityCredential, 0, len(cfg.Credentials))
	for _, template := range cfg.Credentials {
		creds = append(creds, FederatedIdentityCredential{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
				SubscriptionID: cfg.SubscriptionID,
				ResourceGroup:  cfg.ResourceGroup,
				IdentityName:   template.IdentityName,
				CredentialName: formatSubstitutions(template.CredentialName, subs),
			},
			Issuer:    cfg.Issuer,
			Subject:   formatSubstitutions(template.Subject, subs),
			Audiences: append([]string{}, template.Audiences...),
		})
	}
	return creds
}

// previewWorkloadIdentityConfig resolves a project's workload-identity config for
// preview federation, applying the same issuer fallback the standby reconcile
// uses (project metadata, else the service issuer, else the projected SA token's
// issuer). ok=false means the project opts out (no runner_standby_workload_identity).
// Issuer is only required for upsert (delete addresses the credential by ref).
func (s RunnerWorkloadIdentityService) previewWorkloadIdentityConfig(project Project, requireIssuer bool) (runnerWorkloadIdentityConfig, bool, error) {
	cfg, ok, err := runnerWorkloadIdentityConfigFromProject(project)
	if !ok || err != nil {
		return cfg, ok, err
	}
	cfg.Issuer = firstNonEmpty(cfg.Issuer, strings.TrimSpace(s.Issuer), issuerFromServiceAccountToken(s.ServiceAccountTokenPath))
	if requireIssuer && cfg.Issuer == "" {
		return cfg, true, errors.New("runner workload identity requires issuer or RUNNER_WORKLOAD_IDENTITY_ISSUER")
	}
	return cfg, true, nil
}

// EnsurePreviewWorkloadIdentity upserts the federated identity credential(s) a
// preview environment's backend needs to assume the app's Azure identity from its
// own namespace. No-op for projects without runner_standby_workload_identity.
// Idempotent (Azure upsert), so it is safe to call on every (re)provision. Azure
// caps federated identity credentials per managed identity; a live preview
// consumes one slot per credential template until it is deprovisioned.
func (s RunnerWorkloadIdentityService) EnsurePreviewWorkloadIdentity(ctx context.Context, project Project, previewName string) error {
	previewName = strings.TrimSpace(previewName)
	if previewName == "" {
		return errors.New("preview name is required for workload identity federation")
	}
	cfg, ok, err := s.previewWorkloadIdentityConfig(project, true)
	if !ok {
		return nil
	}
	if err != nil {
		return err
	}
	if s.Client == nil {
		return errors.New("runner workload identity client not configured")
	}
	creds := previewWorkloadIdentityCredentials(cfg, previewName)
	// Bound the Azure per-identity cap deliberately, BEFORE any upsert, so the
	// failure is atomic (no partial federation) and surfaced as a clear durable
	// provision error rather than an opaque Azure 400 on the over-cap upsert.
	for _, cred := range creds {
		current, lerr := s.Client.ListFederatedIdentityCredentials(ctx, cred.FederatedIdentityCredentialRef)
		if lerr != nil {
			metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityEnsure, metrics.LivePreviewWorkloadIdentityError)
			return fmt.Errorf("list federated identity credentials for %s: %w", cred.IdentityName, lerr)
		}
		// An already-present credential (re-provision) consumes no new slot.
		if !containsCredentialName(current, cred.CredentialName) && len(current) >= maxFederatedIdentityCredentialsPerIdentity {
			metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityEnsure, metrics.LivePreviewWorkloadIdentityCapExceeded)
			return fmt.Errorf("preview would exceed the %d federated identity credential cap on identity %q (%d in use); deprovision an existing preview for this app or reduce its standby pool",
				maxFederatedIdentityCredentialsPerIdentity, cred.IdentityName, len(current))
		}
	}
	for _, cred := range creds {
		if uerr := s.Client.UpsertFederatedIdentityCredential(ctx, cred); uerr != nil {
			metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityEnsure, metrics.LivePreviewWorkloadIdentityError)
			return fmt.Errorf("upsert preview federated identity credential %s/%s: %w", cred.IdentityName, cred.CredentialName, uerr)
		}
	}
	metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityEnsure, metrics.LivePreviewWorkloadIdentityOK)
	return nil
}

// containsCredentialName reports whether a credential with the given name is
// already present in the list (an idempotent re-provision must not be counted
// against the cap as if it were a new credential).
func containsCredentialName(creds []FederatedIdentityCredential, name string) bool {
	for _, cred := range creds {
		if cred.CredentialName == name {
			return true
		}
	}
	return false
}

// RemovePreviewWorkloadIdentity deletes the preview's federated identity
// credential(s) so a torn-down or failed preview never leaks a credential against
// the app identity. No-op for projects without runner_standby_workload_identity;
// best-effort across multiple credentials (returns the first delete error).
func (s RunnerWorkloadIdentityService) RemovePreviewWorkloadIdentity(ctx context.Context, project Project, previewName string) error {
	previewName = strings.TrimSpace(previewName)
	if previewName == "" {
		return nil
	}
	cfg, ok, err := s.previewWorkloadIdentityConfig(project, false)
	if !ok || err != nil {
		return err
	}
	if s.Client == nil {
		return errors.New("runner workload identity client not configured")
	}
	var firstErr error
	for _, cred := range previewWorkloadIdentityCredentials(cfg, previewName) {
		if derr := s.Client.DeleteFederatedIdentityCredential(ctx, cred.FederatedIdentityCredentialRef); derr != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete preview federated identity credential %s/%s: %w", cred.IdentityName, cred.CredentialName, derr)
		}
	}
	if firstErr != nil {
		// A failed delete is a potential credential leak — count it as an error so
		// it is visible (the orphan sweep is the self-heal backstop).
		metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityRemove, metrics.LivePreviewWorkloadIdentityError)
	} else {
		metrics.RecordLivePreviewWorkloadIdentity(metrics.LivePreviewWorkloadIdentityRemove, metrics.LivePreviewWorkloadIdentityOK)
	}
	return firstErr
}

// previewNamespaceFromSubject extracts the Kubernetes namespace from an AKS
// workload-identity subject of the form "system:serviceaccount:<namespace>:<sa>"
// (a preview's namespace IS its name). Returns "" for any other subject shape, so
// non-AKS subjects are never treated as preview credentials.
func previewNamespaceFromSubject(subject string) string {
	parts := strings.Split(strings.TrimSpace(subject), ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return ""
	}
	return parts[2]
}

// credentialMatchesPreview reports whether cred is EXACTLY one of the credentials
// Glimmung would mint for a preview named previewName under this project's
// templates (matched on identity, credential name, and subject). It is the
// ownership proof the orphan sweep requires before deleting anything: a credential
// Glimmung did not mint will not match and is never touched.
func credentialMatchesPreview(cred FederatedIdentityCredential, cfg runnerWorkloadIdentityConfig, previewName string) bool {
	for _, want := range previewWorkloadIdentityCredentials(cfg, previewName) {
		if want.IdentityName == cred.IdentityName &&
			want.CredentialName == cred.CredentialName &&
			want.Subject == cred.Subject {
			return true
		}
	}
	return false
}

// reclaimOrphanedPreviewCredentials deletes this project's preview federated
// identity credentials whose preview environment no longer exists (liveNames is
// the set of live preview env names). It never touches standby-pool credentials
// (owned by the standby reconcile) or credentials Glimmung did not mint. Returns
// the number reclaimed.
func (s RunnerWorkloadIdentityService) reclaimOrphanedPreviewCredentials(ctx context.Context, project Project, liveNames map[string]bool) (int, error) {
	cfg, ok, err := s.previewWorkloadIdentityConfig(project, false)
	if !ok || err != nil {
		return 0, err
	}
	if s.Client == nil {
		return 0, errors.New("runner workload identity client not configured")
	}
	reclaimed := 0
	var firstErr error
	seenIdentity := map[string]bool{}
	for _, template := range cfg.Credentials {
		if seenIdentity[template.IdentityName] {
			continue
		}
		seenIdentity[template.IdentityName] = true
		current, lerr := s.Client.ListFederatedIdentityCredentials(ctx, FederatedIdentityCredentialRef{
			SubscriptionID: cfg.SubscriptionID,
			ResourceGroup:  cfg.ResourceGroup,
			IdentityName:   template.IdentityName,
		})
		if lerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list federated identity credentials for %s: %w", template.IdentityName, lerr)
			}
			continue
		}
		for _, cred := range current {
			// Standby-pool credentials belong to the standby reconcile — never ours.
			if _, isStandby := managedWorkloadIdentityCredentialSlotName(cred, cfg); isStandby {
				continue
			}
			previewName := previewNamespaceFromSubject(cred.Subject)
			if previewName == "" || !credentialMatchesPreview(cred, cfg, previewName) {
				continue // not a Glimmung-minted preview credential — leave it alone
			}
			if liveNames[previewName] {
				continue // a live preview owns it — desired
			}
			if derr := s.Client.DeleteFederatedIdentityCredential(ctx, cred.FederatedIdentityCredentialRef); derr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("delete orphaned preview credential %s/%s: %w", cred.IdentityName, cred.CredentialName, derr)
				}
				continue
			}
			metrics.RecordLivePreviewWorkloadIdentityOrphanReclaimed()
			reclaimed++
		}
	}
	return reclaimed, firstErr
}

// ReclaimOrphanedPreviewWorkloadIdentities is the one-shot startup sweep that
// reclaims preview federated identity credentials whose preview environment no
// longer exists — the self-heal for a teardown missed because a process died
// mid-deprovision. It mirrors RecoverInFlightTestSlots (startup, not a polling
// loop; idempotent deletes) and the lifecycle's orphan-cleanup model
// (docs/test-slot-lifecycle.md): a federated credential is a per-environment
// preliminary resource, and a missed teardown converges on the next boot. It is
// control-plane-only (the caller gates on ControlPlaneLoopsEnabled) and a no-op
// when workload-identity federation is unconfigured.
func ReclaimOrphanedPreviewWorkloadIdentities(ctx context.Context, store ReadStore, wi RunnerWorkloadIdentityService, logf func(string, ...any)) {
	if wi.Client == nil {
		return
	}
	previewStore, ok := store.(PreviewControlStore)
	if !ok || previewStore == nil {
		return
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		if logf != nil {
			logf("preview workload identity orphan sweep: list projects failed: %v", err)
		}
		return
	}
	previews, err := previewStore.ListPreviewEnvironments(ctx)
	if err != nil {
		if logf != nil {
			logf("preview workload identity orphan sweep: list preview environments failed: %v", err)
		}
		return
	}
	liveByProject := map[string]map[string]bool{}
	for _, env := range previews {
		if liveByProject[env.Project] == nil {
			liveByProject[env.Project] = map[string]bool{}
		}
		liveByProject[env.Project][env.Name] = true
	}
	total := 0
	for _, project := range projects {
		key := firstNonEmpty(project.Name, project.ID)
		reclaimed, rerr := wi.reclaimOrphanedPreviewCredentials(ctx, project, liveByProject[key])
		total += reclaimed
		if rerr != nil && logf != nil {
			logf("preview workload identity orphan sweep: project=%s err=%v", key, rerr)
		}
		if reclaimed > 0 && logf != nil {
			logf("preview workload identity orphan sweep: project=%s reclaimed=%d orphaned credential(s)", key, reclaimed)
		}
	}
	if total > 0 && logf != nil {
		logf("preview workload identity orphan sweep: reclaimed %d orphaned preview credential(s) total", total)
	}
}
