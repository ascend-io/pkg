/*
Copyright 2022 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package login

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/ascend-io/pkg/oci"
	"github.com/ascend-io/pkg/oci/auth/aws"
	"github.com/ascend-io/pkg/oci/auth/azure"
	"github.com/ascend-io/pkg/oci/auth/gcp"
	"github.com/fluxcd/pkg/cache"
)

// ImageRegistryProvider analyzes the provided registry and returns the identified
// container image registry provider.
func ImageRegistryProvider(url string, ref name.Reference) oci.Provider {
	// If the url is a repository root address, use it to analyze. Else, derive
	// the registry from the name reference.
	// NOTE: This is because name.Reference of a repository root assumes that
	// the reference is an image name and defaults to using index.docker.io as
	// the registry host.
	addr := strings.TrimSuffix(url, "/")
	if strings.ContainsRune(addr, '/') {
		addr = ref.Context().RegistryStr()
	}

	_, _, ok := aws.ParseRegistry(addr)
	if ok {
		return oci.ProviderAWS
	}
	if gcp.ValidHost(addr) {
		return oci.ProviderGCP
	}
	if azure.ValidHost(addr) {
		return oci.ProviderAzure
	}
	return oci.ProviderGeneric
}

// ProviderOptions contains options for registry provider login.
type ProviderOptions struct {
	// AwsAutoLogin enables automatic attempt to get credentials for images in
	// ECR.
	AwsAutoLogin bool
	// GcpAutoLogin enables automatic attempt to get credentials for images in
	// GCP.
	GcpAutoLogin bool
	// AzureAutoLogin enables automatic attempt to get credentials for images in
	// ACR.
	AzureAutoLogin bool
	// Cache is a cache for storing auth configurations.
	Cache cache.Expirable[cache.StoreObject[authn.Authenticator]]
}

// Manager is a login manager for various registry providers.
type Manager struct {
	ecr *aws.Client
	gcr *gcp.Client
	acr *azure.Client
}

// Option is a functional option for configuring the manager.
type Option func(*options)

type options struct {
	proxyURL *url.URL
}

// WithProxyURL sets the proxy URL for the manager.
func WithProxyURL(proxyURL *url.URL) Option {
	return func(o *options) {
		o.proxyURL = proxyURL
	}
}

// NewManager initializes a Manager with default registry clients
// configurations.
func NewManager(opts ...Option) *Manager {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	var awsOpts []aws.Option
	var gcpOpts []gcp.Option
	var azureOpts []azure.Option

	if o.proxyURL != nil {
		awsOpts = append(awsOpts, aws.WithProxyURL(o.proxyURL))
		gcpOpts = append(gcpOpts, gcp.WithProxyURL(o.proxyURL))
		azureOpts = append(azureOpts, azure.WithProxyURL(o.proxyURL))
	}

	return &Manager{
		ecr: aws.NewClient(awsOpts...),
		gcr: gcp.NewClient(gcpOpts...),
		acr: azure.NewClient(azureOpts...),
	}
}

// WithECRClient allows overriding the default ECR client.
func (m *Manager) WithECRClient(c *aws.Client) *Manager {
	m.ecr = c
	return m
}

// WithGCRClient allows overriding the default GCR client.
func (m *Manager) WithGCRClient(c *gcp.Client) *Manager {
	m.gcr = c
	return m
}

// WithACRClient allows overriding the default ACR client.
func (m *Manager) WithACRClient(c *azure.Client) *Manager {
	m.acr = c
	return m
}

// Login performs authentication against a registry and returns the Authenticator.
// For generic registry provider, it is no-op.
func (m *Manager) Login(ctx context.Context, url string, ref name.Reference, opts ProviderOptions) (authn.Authenticator, error) {
	provider := ImageRegistryProvider(url, ref)
	var (
		key string
		err error
	)
	if opts.Cache != nil {
		key, err = m.keyFromURL(url, provider)
		if err != nil {
			logr.FromContextOrDiscard(ctx).Error(err, "failed to get cache key")
		} else {
			auth, exists, err := getObjectFromCache(opts.Cache, key)
			if err != nil {
				logr.FromContextOrDiscard(ctx).Error(err, "failed to get auth object from cache")
			}
			if exists {
				return auth, nil
			}
		}
	}

	switch provider {
	case oci.ProviderAWS:
		auth, expiresAt, err := m.ecr.LoginWithExpiry(ctx, opts.AwsAutoLogin, url)
		if err != nil {
			return nil, err
		}
		if opts.Cache != nil {
			err := cacheObject(opts.Cache, auth, key, expiresAt)
			if err != nil {
				logr.FromContextOrDiscard(ctx).Error(err, "failed to cache auth object")
			}
		}
		return auth, nil
	case oci.ProviderGCP:
		auth, expiresAt, err := m.gcr.LoginWithExpiry(ctx, opts.GcpAutoLogin, url, ref)
		if err != nil {
			return nil, err
		}
		if opts.Cache != nil {
			err := cacheObject(opts.Cache, auth, key, expiresAt)
			if err != nil {
				logr.FromContextOrDiscard(ctx).Error(err, "failed to cache auth object")
			}
		}
		return auth, nil
	case oci.ProviderAzure:
		auth, expiresAt, err := m.acr.LoginWithExpiry(ctx, opts.AzureAutoLogin, url, ref)
		if err != nil {
			return nil, err
		}
		if opts.Cache != nil {
			err := cacheObject(opts.Cache, auth, key, expiresAt)
			if err != nil {
				logr.FromContextOrDiscard(ctx).Error(err, "failed to cache auth object")
			}
		}
		return auth, nil
	}
	return nil, nil
}

// OIDCLogin attempts to get an Authenticator for the provided URL endpoint.
//
// If you want to construct an Authenticator based on an image reference,
// you may want to use Login instead.
//
// Deprecated: Use Login instead.
func (m *Manager) OIDCLogin(ctx context.Context, registryURL string, opts ProviderOptions) (authn.Authenticator, error) {
	u, err := url.Parse(registryURL)
	if err != nil {
		return nil, fmt.Errorf("unable to parse registry url: %w", err)
	}
	provider := ImageRegistryProvider(u.Host, nil)
	switch provider {
	case oci.ProviderAWS:
		if !opts.AwsAutoLogin {
			return nil, fmt.Errorf("ECR authentication failed: %w", oci.ErrUnconfiguredProvider)
		}
		logr.FromContextOrDiscard(ctx).Info("logging in to AWS ECR for " + u.Host)
		return m.ecr.OIDCLogin(ctx, u.Host)
	case oci.ProviderGCP:
		if !opts.GcpAutoLogin {
			return nil, fmt.Errorf("GCR authentication failed: %w", oci.ErrUnconfiguredProvider)
		}
		logr.FromContextOrDiscard(ctx).Info("logging in to GCP GCR for " + u.Host)
		return m.gcr.OIDCLogin(ctx)
	case oci.ProviderAzure:
		if !opts.AzureAutoLogin {
			return nil, fmt.Errorf("ACR authentication failed: %w", oci.ErrUnconfiguredProvider)
		}
		logr.FromContextOrDiscard(ctx).Info("logging in to Azure ACR for " + u.Host)
		return m.acr.OIDCLogin(ctx, fmt.Sprintf("%s://%s", u.Scheme, u.Host))
	}
	return nil, nil
}

// keyFromURL returns a key for the cache based on the URL and provider.
// Use this when you don't want to cache the full URL,
// but instead want to cache based on the provider secific way of identifying
// the authentication principal, i.e. the Domain for AWS and Azure, Project for GCP.
func (m *Manager) keyFromURL(ref string, provider oci.Provider) (string, error) {
	if !strings.Contains(ref, "://") {
		ref = fmt.Sprintf("//%s", ref)
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	switch provider {
	case oci.ProviderAWS, oci.ProviderAzure:
		return u.Host, nil
	case oci.ProviderGCP:
		paths := strings.Split(u.Path, "/")
		if len(paths) > 1 {
			return fmt.Sprintf("%s/%s", u.Host, paths[1]), nil
		}
		return u.Host, nil
	}
	return "", nil
}
