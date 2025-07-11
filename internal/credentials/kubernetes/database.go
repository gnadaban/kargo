package kubernetes

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/kelseyhightower/envconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/credentials"
	"github.com/akuity/kargo/internal/credentials/kubernetes/basic"
	"github.com/akuity/kargo/internal/credentials/kubernetes/ecr"
	"github.com/akuity/kargo/internal/credentials/kubernetes/gar"
	"github.com/akuity/kargo/internal/credentials/kubernetes/github"
	"github.com/akuity/kargo/internal/git"
	"github.com/akuity/kargo/internal/helm"
	"github.com/akuity/kargo/internal/logging"
)

// database is an implementation of the credentials.Database interface that
// utilizes a Kubernetes controller runtime client to retrieve credentials
// stored in Kubernetes Secrets.
type database struct {
	controlPlaneClient  client.Client
	localClusterClient  client.Client
	credentialProviders []credentials.Provider
	cfg                 DatabaseConfig
}

// DatabaseConfig represents configuration for a Kubernetes based implementation
// of the credentials.Database interface.
type DatabaseConfig struct {
	GlobalCredentialsNamespaces []string `envconfig:"GLOBAL_CREDENTIALS_NAMESPACES" default:""`
	AllowCredentialsOverHTTP    bool     `envconfig:"ALLOW_CREDENTIALS_OVER_HTTP" default:"false"`
}

func DatabaseConfigFromEnv() DatabaseConfig {
	cfg := DatabaseConfig{}
	envconfig.MustProcess("", &cfg)
	slices.Sort(cfg.GlobalCredentialsNamespaces)
	return cfg
}

// NewDatabase initializes and returns an implementation of the
// credentials.Database interface that utilizes a Kubernetes controller runtime
// client to retrieve Credentials stored in Kubernetes Secrets.
func NewDatabase(
	ctx context.Context,
	controlPlaneClient client.Client,
	localClusterClient client.Client,
	cfg DatabaseConfig,
) credentials.Database {
	var credentialProviders = []credentials.Provider{
		&basic.CredentialProvider{},
		ecr.NewAccessKeyProvider(),
		ecr.NewManagedIdentityProvider(ctx),
		gar.NewServiceAccountKeyProvider(),
		gar.NewWorkloadIdentityFederationProvider(ctx),
		github.NewAppCredentialProvider(),
	}

	db := &database{
		controlPlaneClient: controlPlaneClient,
		localClusterClient: localClusterClient,
		cfg:                cfg,
	}

	for _, p := range credentialProviders {
		if p != nil {
			db.credentialProviders = append(db.credentialProviders, p)
		}
	}

	return db
}

func (k *database) Get(
	ctx context.Context,
	namespace string,
	credType credentials.Type,
	repoURL string,
) (*credentials.Credentials, error) {
	// If we are dealing with an insecure HTTP endpoint (of any type),
	// refuse to return any credentials
	if !k.cfg.AllowCredentialsOverHTTP && strings.HasPrefix(repoURL, "http://") {
		logging.LoggerFromContext(ctx).Info(
			"refused to get credentials for insecure HTTP endpoint",
			"repoURL", repoURL,
		)
		return nil, nil
	}

	clients := make([]client.Client, 1, 2)
	clients[0] = k.controlPlaneClient
	if k.localClusterClient != nil {
		clients = append(clients, k.localClusterClient)
	}

	var secret *corev1.Secret
	var err error

clientLoop:
	for _, c := range clients {
		// Check namespace for credentials
		if secret, err = k.getCredentialsSecret(
			ctx,
			c,
			namespace,
			credType,
			repoURL,
		); err != nil {
			return nil, err
		}
		if secret != nil {
			break clientLoop
		}
		// Check global credentials namespaces for credentials
		for _, globalCredsNamespace := range k.cfg.GlobalCredentialsNamespaces {
			if secret, err = k.getCredentialsSecret(
				ctx,
				c,
				globalCredsNamespace,
				credType,
				repoURL,
			); err != nil {
				return nil, err
			}
			if secret != nil {
				break clientLoop
			}
		}
	}

	var data map[string][]byte
	if secret != nil {
		data = secret.Data
	}

	for _, p := range k.credentialProviders {
		creds, err := p.GetCredentials(ctx, namespace, credType, repoURL, data)
		if err != nil {
			return nil, err
		}
		if creds != nil {
			return creds, nil
		}
	}

	return nil, nil
}

func (k *database) getCredentialsSecret(
	ctx context.Context,
	c client.Client,
	namespace string,
	credType credentials.Type,
	repoURL string,
) (*corev1.Secret, error) {
	// List all secrets in the namespace that are labeled with the credential
	// type.
	secrets := corev1.SecretList{}
	if err := c.List(
		ctx,
		&secrets,
		&client.ListOptions{
			Namespace: namespace,
			LabelSelector: labels.Set(map[string]string{
				kargoapi.LabelKeyCredentialType: credType.String(),
			}).AsSelector(),
		},
	); err != nil {
		return nil, err
	}

	// Sort the secrets for consistent ordering every time this function is
	// called.
	slices.SortFunc(secrets.Items, func(lhs, rhs corev1.Secret) int {
		return strings.Compare(lhs.Name, rhs.Name)
	})

	// Note: We formerly applied these normalizations to any URL, thinking them
	// generally safe. We no longer do this as it was discovered that an image
	// repository URL with a port number could be mistaken for an SCP-style URL of
	// the form host.xz:path/to/repo
	switch credType {
	case credentials.TypeGit:
		repoURL = git.NormalizeURL(repoURL)
	case credentials.TypeHelm:
		repoURL = helm.NormalizeChartRepositoryURL(repoURL)
	}

	logger := logging.LoggerFromContext(ctx)

	// Search for a matching Secret.
	for _, secret := range secrets.Items {
		if secret.Data == nil {
			continue
		}

		isRegex := string(secret.Data[credentials.FieldRepoURLIsRegex]) == "true"
		urlBytes, ok := secret.Data[credentials.FieldRepoURL]
		if !ok {
			continue
		}

		if isRegex {
			regex, err := regexp.Compile(string(urlBytes))
			if err != nil {
				logger.Error(
					err, "failed to compile regex for credential secret",
					"namespace", namespace,
					"secret", secret.Name,
				)
				continue
			}
			if regex.MatchString(repoURL) {
				return &secret, nil
			}
			continue
		}

		// Not a regex
		secretURL := string(urlBytes)
		switch credType {
		case credentials.TypeGit:
			secretURL = git.NormalizeURL(secretURL)
		case credentials.TypeHelm:
			secretURL = helm.NormalizeChartRepositoryURL(secretURL)
		}
		if secretURL == repoURL {
			return &secret, nil
		}
	}
	return nil, nil
}
