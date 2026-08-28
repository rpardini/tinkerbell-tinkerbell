package webhttp

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/ui/templates"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TinkerbellResource defines a Tinkerbell CRD resource for permission checking.
type TinkerbellResource struct {
	Resource string
	Group    string
}

// TinkerbellResources defines all Tinkerbell CRD resources to check permissions for.
// Exported so templates can access the list.
var TinkerbellResources = []TinkerbellResource{
	{resourceHardware, groupTinkerbell},
	{"templates", groupTinkerbell},
	{resourceWorkflows, groupTinkerbell},
	{"workflowrulesets", groupTinkerbell},
	{"machines", groupBMC},
	{"jobs", groupBMC},
	{resourceTasks, groupBMC},
}

// tinkerbellVerbs defines the verbs to check for each resource.
var tinkerbellVerbs = []string{"get", verbList, "watch", "create", "update", "patch", "delete"}

// HandlePermissions handles the permissions page showing user's Tinkerbell RBAC permissions.
// The page loads immediately with loading indicators, then fetches each resource's permissions via HTMX.
func HandlePermissions(c *gin.Context, log logr.Logger) {
	ctx := c.Request.Context()
	client, err := GetKubeClientFromGinContext(c)
	if err != nil {
		log.Error(err, "Failed to get kube client")
		if HandleAuthError(c, err, log) {
			return
		}
		c.String(500, "Internal server error")
		return
	}

	namespaces := GetKubeNamespaces(ctx, c, client, log)

	// Convert to template-friendly format
	resources := make([]templates.ResourceInfo, len(TinkerbellResources))
	for i, r := range TinkerbellResources {
		resources[i] = templates.ResourceInfo{
			Resource: r.Resource,
			APIGroup: r.Group,
		}
	}

	cfg := templates.PageConfig{
		BaseURL:    GetBaseURL(c),
		Namespaces: namespaces,
	}
	component := templates.PermissionsPage(cfg, resources)
	c.Header("Content-Type", "text/html")
	RenderComponent(ctx, c.Writer, component, log)
}

// getSANamespace returns the service account namespace stored in the gin
// context by the auth middleware, or "" if not set.
func getSANamespace(c *gin.Context) string {
	if ns, exists := c.Get(cookieNameSANamespace); exists {
		if nsStr, ok := ns.(string); ok {
			return nsStr
		}
	}
	return ""
}

// HandlePermissionCheck handles checking permissions for a single resource.
// Called via HTMX to progressively load permission status for each resource.
func HandlePermissionCheck(c *gin.Context, log logr.Logger) {
	ctx := c.Request.Context()
	client, err := GetKubeClientFromGinContext(c)
	if err != nil {
		log.Error(err, "Failed to get kube client")
		// Return an error row
		component := templates.PermissionRowError(c.Param("resource"), c.Query("group"))
		c.Header("Content-Type", "text/html")
		RenderComponent(ctx, c.Writer, component, log)
		return
	}

	resource := c.Param("resource")
	group := c.Query("group")

	// Check permissions for this resource
	perm := getResourcePermissions(ctx, client, log, resource, group, getSANamespace(c))

	component := templates.PermissionRow(perm)
	c.Header("Content-Type", "text/html")
	RenderComponent(ctx, c.Writer, component, log)
}

// getResourcePermissions checks permissions for a single resource.
func getResourcePermissions(ctx context.Context, client *KubeClient, log logr.Logger, resource, group, namespace string) templates.Permission {
	var allowedVerbs []string
	permNamespace := "" // Will be set to namespace if only namespace-scoped access is allowed

	for _, verb := range tinkerbellVerbs {
		// First try cluster-wide permission
		sar := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Verb:     verb,
					Group:    group,
					Resource: resource,
				},
			},
		}

		result, err := client.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
		if err != nil {
			log.V(1).Info("Failed to check permission", "resource", resource, "verb", verb, "error", err)
			continue
		}

		if result.Status.Allowed {
			allowedVerbs = append(allowedVerbs, verb)
			continue
		}

		// If cluster-wide not allowed and we have a namespace, try namespace-scoped
		if namespace != "" {
			sar.Spec.ResourceAttributes.Namespace = namespace
			result, err = client.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
			if err != nil {
				log.V(1).Info("Failed to check namespace-scoped permission", "resource", resource, "verb", verb, "namespace", namespace, "error", err)
				continue
			}

			if result.Status.Allowed {
				allowedVerbs = append(allowedVerbs, verb)
				permNamespace = namespace // Mark as namespace-scoped
			}
		}
	}

	return templates.Permission{
		Resource:  resource,
		APIGroup:  group,
		Namespace: permNamespace,
		Verbs:     allowedVerbs,
	}
}

// canUpdateWorkflows reports whether the current user can update
// workflows.tinkerbell.org, checking cluster-wide access first and falling
// back to namespace-scoped access. Used to disable the Enable action
// proactively instead of letting it fail with a 403 on click.
func canUpdateWorkflows(ctx context.Context, client *KubeClient, log logr.Logger, namespace string) bool {
	if client.clientset == nil {
		// Fail closed: no clientset (e.g. a test double lacking one) means we
		// can't confirm access, so don't offer an action that would 403.
		return false
	}

	sar := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Verb:     "update",
				Group:    groupTinkerbell,
				Resource: resourceWorkflows,
			},
		},
	}

	result, err := client.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		log.V(1).Info("Failed to check update permission on workflows", "error", err)
		return false
	}
	if result.Status.Allowed {
		return true
	}
	if namespace == "" {
		return false
	}

	sar.Spec.ResourceAttributes.Namespace = namespace
	result, err = client.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		log.V(1).Info("Failed to check namespace-scoped update permission on workflows", "namespace", namespace, "error", err)
		return false
	}
	return result.Status.Allowed
}
