# ============================================================================
# Workload identity for glimmung
# ============================================================================
# Glimmung previously reused `infra-shared-identity` (provisioned in
# infra-bootstrap) via a per-app federated credential keyed on
# `system:serviceaccount:glimmung:infra-shared`. That shared identity holds
# broad data-plane roles shared across opted-in apps.
#
# Glimmung-owned identities live in the `glimmung` resource group:
# - `glimmung-identity` for the API/dashboard pod
# - `glimmung-native-runner-identity` for native Kubernetes runner Jobs
# - `glimmung-provider-api-proxy-identity` for provider OAuth proxy KV writeback
#
# Federated credentials use exact-match subjects. The API/dashboard pod
# remains on `system:serviceaccount:glimmung:infra-shared` to avoid a
# chart/service-account rename in this infra slice. Native runner Jobs use
# `system:serviceaccount:glimmung-runs:glimmung-native-runner`.
# ============================================================================

data "azurerm_resource_group" "infra" {
  name = local.infra.resource_group_name
}

resource "azurerm_resource_group" "glimmung" {
  name     = "glimmung"
  location = data.azurerm_resource_group.infra.location
}

data "azurerm_container_registry" "romaine" {
  name                = "romainecr"
  resource_group_name = local.infra.resource_group_name
}

resource "azurerm_user_assigned_identity" "glimmung_dedicated" {
  name                = "glimmung-identity"
  resource_group_name = azurerm_resource_group.glimmung.name
  location            = azurerm_resource_group.glimmung.location
}

resource "azurerm_user_assigned_identity" "native_runner" {
  name                = "glimmung-native-runner-identity"
  resource_group_name = azurerm_resource_group.glimmung.name
  location            = azurerm_resource_group.glimmung.location
}

resource "azurerm_user_assigned_identity" "provider_api_proxy" {
  name                = "glimmung-provider-api-proxy-identity"
  resource_group_name = azurerm_resource_group.glimmung.name
  location            = azurerm_resource_group.glimmung.location
}

resource "azurerm_federated_identity_credential" "glimmung_dedicated" {
  name                = "aks-glimmung"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.glimmung_dedicated.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:glimmung:infra-shared"
}

resource "azurerm_federated_identity_credential" "native_runner" {
  name                = "aks-glimmung-native-runner"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.native_runner.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:glimmung-runs:glimmung-native-runner"
}

resource "azurerm_federated_identity_credential" "provider_api_proxy" {
  name                = "aks-glimmung-provider-api-proxy"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.provider_api_proxy.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:glimmung-runs:glimmung-provider-api-proxy"
}

resource "azurerm_role_assignment" "native_runner_acr_push" {
  scope                = data.azurerm_container_registry.romaine.id
  role_definition_name = "AcrPush"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

# Native app runners use `az acr build` for validation images because the
# Kubernetes job does not have a Docker daemon. AcrPush covers direct image
# push/pull, but ACR Tasks are management-plane operations on the registry.
resource "azurerm_role_assignment" "native_runner_acr_build_contributor" {
  scope                = data.azurerm_container_registry.romaine.id
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

resource "azurerm_role_assignment" "glimmung_dedicated_subscription_contributor" {
  scope                = "/subscriptions/${data.azurerm_client_config.current.subscription_id}"
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_role_assignment" "glimmung_dedicated_subscription_rbac_admin" {
  scope                = "/subscriptions/${data.azurerm_client_config.current.subscription_id}"
  role_definition_name = "Role Based Access Control Administrator"
  principal_id         = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_role_assignment" "provider_api_proxy_keyvault" {
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Secrets Officer"
  principal_id         = azurerm_user_assigned_identity.provider_api_proxy.principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_key_vault_secret" "provider_api_proxy_client_id" {
  name         = "glimmung-provider-api-proxy-mi-client-id"
  value        = azurerm_user_assigned_identity.provider_api_proxy.client_id
  key_vault_id = azurerm_key_vault.main.id
}

resource "azurerm_key_vault_secret" "tenant_id" {
  name         = "glimmung-tenant-id"
  value        = data.azurerm_client_config.current.tenant_id
  key_vault_id = azurerm_key_vault.main.id
}

output "glimmung_dedicated_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung_dedicated.client_id
  description = "client_id of the Glimmung-owned glimmung-identity. Pin this into k8s/values.yaml."
}

output "glimmung_native_runner_identity_client_id" {
  value       = azurerm_user_assigned_identity.native_runner.client_id
  description = "client_id of glimmung-native-runner-identity. Use for the glimmung-runs runner ServiceAccount annotation."
}

output "glimmung_provider_api_proxy_identity_client_id" {
  value       = azurerm_user_assigned_identity.provider_api_proxy.client_id
  description = "client_id of glimmung-provider-api-proxy-identity. Use for the provider API proxy ServiceAccount annotation."
}
