package migrate

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const argoNamespace = "stakater-tenant-operator"

// The fake cluster is shaped to cover every case the runbook calls out, because
// the interesting behaviour is per-case:
//
//	pe      Extensions CR, purge=true, per-tenant overrides -- the full cutover path
//	sos     Extensions CR, no overrides, already opted in
//	legacy  no Extensions CR, an AppProject carrying a v1beta2 Tenant ownerRef
//	newbie  no Extensions CR and no AppProject
//
// plus one Extensions CR naming a tenant that does not exist, which produced
// nothing but still holds the finalizer that blocks removing the CRD.

const defaultIntegrationConfig = `{
  "apiVersion": "tenantoperator.stakater.com/v1beta1",
  "kind": "IntegrationConfig",
  "metadata": {"name": "tenant-operator-config", "namespace": "multi-tenant-operator"},
  "spec": {
    "accessControl": {"privileged": {"namespaces": ["^openshift.*"], "groups": ["cluster-admins"]}},
    "integrations": {"argocd": {
      "namespace": "stakater-tenant-operator",
      "clusterResourceWhitelist": [{"group": "tronador.stakater.com", "kind": "EnvironmentProvisioner"}],
      "namespaceResourceBlacklist": [{"group": "", "kind": "ResourceQuota"}]
    }}
  }
}`

const defaultExtensions = `{"items": [
  {"metadata": {"name": "pe-extensions"},
   "spec": {"tenantName": "pe", "argoCD": {
     "onDeletePurgeAppProject": true,
     "appProject": {
       "sourceRepos": ["https://github.com/acme/pe-gitops"],
       "namespaceResourceBlacklist": []
     }}}},
  {"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}},
  {"metadata": {"name": "ghost-extensions"}, "spec": {"tenantName": "nosuchtenant"}}
]}`

const defaultTenants = `{"items": [
  {"metadata": {"name": "pe"},
   "spec": {"accessControl": {"owners": {"groups": ["pe-admins"], "users": ["a@x.io"]}}}},
  {"metadata": {"name": "sos",
    "annotations": {"argocd.tenantoperator.stakater.com/enabled": "true"}}, "spec": {}},
  {"metadata": {"name": "legacy"}, "spec": {}},
  {"metadata": {"name": "newbie"}, "spec": {}}
]}`

// AppProject/legacy carries two ownerReferences on purpose. Stripping the whole
// list is the easy mistake, and it would sever an unrelated ownership the
// migration has no business touching.
const defaultAppProjects = `{"items": [
  {"metadata": {"name": "pe", "namespace": "stakater-tenant-operator"},
   "spec": {
     "description": "Managed by Extensions controller",
     "sourceRepos": ["https://github.com/acme/pe-gitops"],
     "destinations": [
       {"namespace": "pe-b", "server": "https://kubernetes.default.svc"},
       {"namespace": "pe-a", "server": "https://kubernetes.default.svc"}
     ],
     "roles": [{"name": "pe-owner", "groups": ["pe-owner-group-489fab"],
                "policies": ["p, proj:pe:pe-owner, *, *, pe/*, allow"]}]
   }},
  {"metadata": {"name": "sos", "namespace": "stakater-tenant-operator"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}},
  {"metadata": {"name": "legacy", "namespace": "stakater-tenant-operator",
    "ownerReferences": [
      {"apiVersion": "tenantoperator.stakater.com/v1beta2", "kind": "Tenant",
       "name": "legacy", "uid": "aaa", "controller": true},
      {"apiVersion": "v1", "kind": "ConfigMap", "name": "keep-me", "uid": "bbb"}
    ]},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}}
]}`

// seededLists renders the chart with the same two resource lists the fixture
// IntegrationConfig carries, which is what `run` compares to decide whether
// step 2 is done.
const seededLists = `
argocd:
  clusterResourceWhitelist:
    - group: tronador.stakater.com
      kind: EnvironmentProvisioner
  namespaceResourceBlacklist:
    - group: ""
      kind: ResourceQuota
`

// The cluster at successive points in the migration, for driving `run` forward
// one step at a time. Each satisfies the step before it.

// After the override annotations have been committed: no CR sets an override,
// so step 3 is done. purge is still on, so step 4 is not.
const extensionsWithoutOverrides = `{"items": [
  {"metadata": {"name": "pe-extensions"},
   "spec": {"tenantName": "pe", "argoCD": {"onDeletePurgeAppProject": true}}},
  {"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}},
  {"metadata": {"name": "ghost-extensions"}, "spec": {"tenantName": "nosuchtenant"}}
]}`

// The purge intent is carried onto the Tenant BEFORE the CR's flag is turned
// off -- otherwise flipping the flag would lose the tenant's stated intent, which
// matters the day TOV2-1 makes retain enforceable. So reaching step 4 requires
// the annotation to be present already.
var tenantsWithPurgeIntentCarried = strings.Replace(defaultTenants,
	`{"metadata": {"name": "pe"},`,
	`{"metadata": {"name": "pe", "annotations":
	   {"argocd.tenantoperator.stakater.com/on-delete-purge": "true"}},`, 1)

// After the purge flip: steps 3 and 4 are both done.
const extensionsRetained = `{"items": [
  {"metadata": {"name": "pe-extensions"},
   "spec": {"tenantName": "pe", "argoCD": {"onDeletePurgeAppProject": false}}},
  {"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}},
  {"metadata": {"name": "ghost-extensions"}, "spec": {"tenantName": "nosuchtenant"}}
]}`

// After strip-owner-refs: step 5 is done, so `run` moves on to the snapshot.
var appProjectsNoLegacyOwner = strings.Replace(defaultAppProjects,
	`"ownerReferences": [
      {"apiVersion": "tenantoperator.stakater.com/v1beta2", "kind": "Tenant",
       "name": "legacy", "uid": "aaa", "controller": true},
      {"apiVersion": "v1", "kind": "ConfigMap", "name": "keep-me", "uid": "bbb"}
    ]`,
	`"ownerReferences": [{"apiVersion": "v1", "kind": "ConfigMap", "name": "keep-me", "uid": "bbb"}]`, 1)

// yamlToJSON converts a single-document YAML manifest to JSON, which is what the
// kubectl shim serves.
func yamlToJSON(in []byte) (string, error) {
	var doc any
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}
	if doc == nil {
		return "", fmt.Errorf("manifest is empty")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("encoding JSON: %w", err)
	}
	return string(out), nil
}
