/*
Copyright 2019 The Kubernetes Authors.

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

package model

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/go-jose/go-jose/v4"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/fitasks"
	"k8s.io/kops/util/pkg/vfs"
)

// IssuerDiscoveryModelBuilder publish OIDC issuer discovery metadata
type IssuerDiscoveryModelBuilder struct {
	*KopsModelContext

	Lifecycle fi.Lifecycle
	Cluster   *kops.Cluster
}

type oidcDiscovery struct {
	Issuer                string   `json:"issuer"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	ResponseTypes         []string `json:"response_types_supported"`
	SubjectTypes          []string `json:"subject_types_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported       []string `json:"claims_supported"`
}

func (b *IssuerDiscoveryModelBuilder) Build(c *fi.CloudupModelBuilderContext) error {
	ctx := context.TODO()

	said := b.Cluster.Spec.ServiceAccountIssuerDiscovery
	if said == nil || said.DiscoveryStore == "" {
		return nil
	}

	signingKeyTaskObject, found := c.Tasks["Keypair/service-account"]
	if !found {
		return fmt.Errorf("keypair/service-account task not found")
	}

	skTask := signingKeyTaskObject.(*fitasks.Keypair)

	keys := &OIDCKeys{
		SigningKey: skTask,
	}

	discovery, err := buildDiscoveryJSON(*b.Cluster.Spec.KubeAPIServer.ServiceAccountIssuer)
	if err != nil {
		return err
	}

	var publicFileACL *bool

	discoveryStorePath := b.Cluster.Spec.ServiceAccountIssuerDiscovery.DiscoveryStore
	discoveryStore, err := vfs.Context.BuildVfsPath(discoveryStorePath)
	if err != nil {
		return fmt.Errorf("building VFS path for %q: %w", discoveryStorePath, err)
	}

	switch discoveryStore := discoveryStore.(type) {
	case *vfs.S3Path:
		discoveryStoreURL, err := discoveryStore.GetHTTPsUrl(b.Cluster.Spec.IsIPv6Only())
		if err != nil {
			return err
		}
		if discoveryStoreURL == fi.ValueOf(b.Cluster.Spec.KubeAPIServer.ServiceAccountIssuer) {
			// Using Amazon S3 static website hosting requires public access
			isPublic, err := discoveryStore.IsBucketPublic(ctx)
			if err != nil {
				return fmt.Errorf("checking if bucket was public: %w", err)
			}
			if !isPublic {
				klog.Infof("serviceAccountIssuers bucket %q is not public; will use object ACL", discoveryStore.Bucket())
				publicFileACL = fi.PtrTo(true)
			}
		} else {
			klog.Infof("using user managed serviceAccountIssuers")
		}
	case *vfs.GSPath:
		discoveryStoreURL, err := discoveryStore.GetHTTPsUrl()
		if err != nil {
			return err
		}
		if discoveryStoreURL == fi.ValueOf(b.Cluster.Spec.KubeAPIServer.ServiceAccountIssuer) {
			// Using Google Cloud Storage requires public access
			isPublic, err := discoveryStore.IsBucketPublic(ctx)
			if err != nil {
				return fmt.Errorf("checking if bucket was public: %w", err)
			}
			if !isPublic {
				klog.Infof("serviceAccountIssuers bucket %q is not public; will use object ACL", discoveryStore.Bucket())
				publicFileACL = fi.PtrTo(true)
			}
		} else {
			klog.Infof("using user managed serviceAccountIssuers")
		}

	case *vfs.MemFSPath:
		// ok

	default:
		return fmt.Errorf("unhandled type %T", discoveryStore)
	}

	keysFile := &fitasks.ManagedFile{
		Contents:  keys,
		Lifecycle: b.Lifecycle,
		Location:  fi.PtrTo("openid/v1/jwks"),
		Name:      fi.PtrTo("keys.json"),
		Base:      fi.PtrTo(discoveryStorePath),
		PublicACL: publicFileACL,
	}
	c.AddTask(keysFile)

	discoveryFile := &fitasks.ManagedFile{
		Contents:  fi.NewBytesResource(discovery),
		Lifecycle: b.Lifecycle,
		Location:  fi.PtrTo(".well-known/openid-configuration"),
		Name:      fi.PtrTo("discovery.json"),
		Base:      fi.PtrTo(discoveryStorePath),
		PublicACL: publicFileACL,
	}
	c.AddTask(discoveryFile)

	return nil
}

func buildDiscoveryJSON(issuerURL string) ([]byte, error) {
	d := oidcDiscovery{
		Issuer:                issuerURL,
		JWKSURI:               fmt.Sprintf("%v/openid/v1/jwks", issuerURL),
		AuthorizationEndpoint: "urn:kubernetes:programmatic_authorization",
		ResponseTypes:         []string{"id_token"},
		SubjectTypes:          []string{"public"},
		SigningAlgs:           []string{"RS256"},
		ClaimsSupported:       []string{"sub", "iss"},
	}
	return json.MarshalIndent(d, "", "")
}

type KeyResponse struct {
	Keys []jose.JSONWebKey `json:"keys"`
}

type OIDCKeys struct {
	SigningKey *fitasks.Keypair
}

// GetDependencies adds CA to the list of dependencies
func (o *OIDCKeys) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	return []fi.CloudupTask{
		o.SigningKey,
	}
}

func (o *OIDCKeys) Open() (io.Reader, error) {
	keyset := o.SigningKey.Keyset()
	var keys []jose.JSONWebKey

	for _, item := range keyset.Items {
		if item.DistrustTimestamp != nil {
			continue
		}
		if item.Certificate == nil || item.Certificate.Subject.CommonName != "service-account" {
			continue
		}

		publicKey := item.Certificate.PublicKey
		publicKeyDERBytes, err := x509.MarshalPKIXPublicKey(publicKey)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize public key to DER format: %v", err)
		}

		hasher := crypto.SHA256.New()
		hasher.Write(publicKeyDERBytes)
		publicKeyDERHash := hasher.Sum(nil)

		keyID := base64.RawURLEncoding.EncodeToString(publicKeyDERHash)

		keys = append(keys, jose.JSONWebKey{
			Key:       publicKey,
			KeyID:     keyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		})
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].KeyID < keys[j].KeyID
	})

	keyResponse := KeyResponse{Keys: keys}
	jsonBytes, err := json.MarshalIndent(keyResponse, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal json: %w", err)
	}

	return bytes.NewReader(jsonBytes), nil
}
