package ec2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tinkerbell/tinkerbell/pkg/data"
	"github.com/tinkerbell/tinkerbell/tootles/internal/frontend/ec2/internal/staticroute"
	"github.com/tinkerbell/tinkerbell/tootles/internal/ginutil"
	"github.com/tinkerbell/tinkerbell/tootles/internal/http/httperror"
	"github.com/tinkerbell/tinkerbell/tootles/internal/http/request"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ErrInstanceNotFound indicates an instance could not be found for the given identifier.
var ErrInstanceNotFound = errors.New("instance not found")

// Client is a backend for retrieving EC2 Instance data.
type Client interface {
	// GetEC2Instance retrieves an Instance associated with ip. If no Instance can be
	// found, it should return ErrInstanceNotFound.
	GetEC2Instance(_ context.Context, ip string) (data.Ec2Instance, error)
	// GetEC2InstanceByMetadataToken retrieves an Instance associated with a metadata token. If no Instance can be
	// found, it should return ErrInstanceNotFound.
	GetEC2InstanceByMetadataToken(_ context.Context, token string) (data.Ec2Instance, error)
}

// Frontend is an EC2 HTTP API frontend. It is responsible for configuring routers with handlers
// for the AWS EC2 instance metadata API.
type Frontend struct {
	client Client
}

// New creates a new Frontend.
func New(client Client) Frontend {
	return Frontend{
		client: client,
	}
}

// Configure configures router with the supported AWS EC2 instance metadata API endpoints.
//
// TODO(chrisdoherty4) Document unimplemented endpoints.
func (f Frontend) Configure(router gin.IRouter) {
	// Setup the 2009-04-04 API path prefix and use a trailing slash route helper to patch
	// equivalent trailing slash routes.
	v20090404 := ginutil.TrailingSlashRouteHelper{IRouter: router.Group("/2009-04-04")}
	v20090404viaToken := ginutil.TrailingSlashRouteHelper{IRouter: router.Group("/tootles/token/:token/2009-04-04")}

	// Create a static route builder that we can add all data routes to which are the basis for
	// all static routes.
	staticRoutes := staticroute.NewBuilder()

	// Configure all dynamic routes. Dynamic routes are anything that requires retrieving a specific
	// instance and returning data from it.
	for _, r := range dataRoutes {
		v20090404.GET(r.Endpoint, func(ctx *gin.Context) {
			instance, err := f.getInstanceViaIP(ctx, ctx.Request)
			f.handleIfNotError(ctx, err, r.Filter, instance)
		})

		v20090404viaToken.GET(r.Endpoint, func(ctx *gin.Context) {
			instance, err := f.getInstanceViaToken(ctx)
			f.handleIfNotError(ctx, err, r.Filter, instance)
		})

		staticRoutes.FromEndpoint(r.Endpoint)
	}

	staticEndpointBinder := func(router ginutil.TrailingSlashRouteHelper, endpoint string, childEndpoints []string) {
		router.GET(endpoint, func(ctx *gin.Context) {
			ctx.String(http.StatusOK, join(childEndpoints))
		})
	}

	for _, r := range staticRoutes.Build() {
		staticEndpointBinder(v20090404, r.Endpoint, r.Children)
		staticEndpointBinder(v20090404viaToken, r.Endpoint, r.Children)
	}
}

func (f Frontend) handleIfNotError(ctx *gin.Context, err error, filter filterFunc, instance data.Ec2Instance) {
	if err != nil {
		// If there's an error containing an http status code, use that status code else
		// assume its an internal server error.
		var httpErr *httperror.E
		if errors.As(err, &httpErr) {
			_ = ctx.AbortWithError(httpErr.StatusCode, err)
		} else {
			_ = ctx.AbortWithError(http.StatusInternalServerError, err)
		}

		return
	}

	ctx.String(http.StatusOK, filter(instance))
}

// getInstanceViaIP is a gin-specific method for retrieving Instance data based on a remote
// address or a token identifier included in the request path.
func (f Frontend) getInstanceViaIP(ctx *gin.Context, r *http.Request) (data.Ec2Instance, error) {
	// Normal IP based lookup. SNAT, proxies, externalTrafficPolicy:Cluster, misconfigured X-Forwarded-For headers, etc are all in play here.
	ip, err := request.RemoteAddrIP(r)
	if err != nil {
		return data.Ec2Instance{}, httperror.New(http.StatusBadRequest, "invalid remote addr")
	}

	instance, err := f.client.GetEC2Instance(ctx, ip)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) || apierrors.IsNotFound(err) {
			return data.Ec2Instance{}, httperror.New(http.StatusNotFound, fmt.Sprintf("no hardware found for source ip: %s", ip))
		}

		// TODO(chrisdoherty4) What happens when multiple Instance could be returned? What
		// is the behavior of GetEC2Instance?
		return data.Ec2Instance{}, httperror.Wrap(http.StatusInternalServerError, err)
	}

	return instance, nil
}

// getInstanceViaToken is a gin-specific method for retrieving Instance data based on a remote
// address or a token identifier included in the request path.
func (f Frontend) getInstanceViaToken(ctx *gin.Context) (data.Ec2Instance, error) {
	token := ctx.Param("token")
	if strings.TrimSpace(token) == "" {
		return data.Ec2Instance{}, httperror.New(http.StatusNotFound, "token not looked up as it is invalid")
	}

	instance, err := f.client.GetEC2InstanceByMetadataToken(ctx, token)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) || apierrors.IsNotFound(err) {
			return data.Ec2Instance{}, httperror.New(http.StatusNotFound, fmt.Sprintf("no hardware found for source token: '%s'", token))
		}
		return data.Ec2Instance{}, httperror.Wrap(http.StatusInternalServerError, err)
	}

	return instance, nil
}

func join(v []string) string {
	return strings.Join(v, "\n")
}
