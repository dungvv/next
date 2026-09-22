// Package config mirrors the Rust `config::Config` / `macro_env_var` loading:
// environment variables in SCREAMING_SNAKE_CASE, per-environment defaults, and
// local-vs-remote secret resolution (env var value is literal locally and a
// Secrets Manager secret name in dev/prod).
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Environment mirrors macro_env::Environment.
type Environment string

const (
	EnvProduction Environment = "prod"
	EnvDevelop    Environment = "dev"
	EnvLocal      Environment = "local"
)

// EnvironmentFromEnv mirrors Environment::new_or_prod.
func EnvironmentFromEnv() Environment {
	switch Environment(strings.ToLower(strings.TrimSpace(os.Getenv("ENVIRONMENT")))) {
	case EnvDevelop:
		return EnvDevelop
	case EnvLocal:
		return EnvLocal
	default:
		return EnvProduction
	}
}

// staticFileServiceURL mirrors macro_service_urls::StaticFileServiceUrl.
func staticFileServiceURL(env Environment) string {
	if v := os.Getenv("STATIC_FILE_SERVICE_URL"); v != "" {
		return v
	}
	switch env {
	case EnvLocal:
		return "http://localhost:8100"
	case EnvDevelop:
		return "https://static-file-service-dev.macro.com"
	default:
		return "https://static-file-service.macro.com"
	}
}

// s3EventQueue mirrors macro_queues::StaticFileServiceS3EventQueueUrl:
// OVERRIDE_STATIC_FILE_SERVICE_S3_EVENT_QUEUE_URL wins, else the per-env
// default (a queue name in this binary's table; deployed environments inject
// a full queue URL through the override).
func s3EventQueue(env Environment) string {
	if v := os.Getenv("OVERRIDE_STATIC_FILE_SERVICE_S3_EVENT_QUEUE_URL"); v != "" {
		return v
	}
	switch env {
	case EnvLocal:
		return "static-file-s3-event-notification-queue"
	case EnvDevelop:
		return "static-file-s3-event-notification-queue-dev"
	default:
		return "static-file-s3-event-notification-queue-prod"
	}
}

// Config mirrors static_file_service::config::Config plus the JWT validation
// args resolved by main in Rust.
type Config struct {
	Environment            Environment
	Port                   int
	DynamoDBTableName      string
	StaticStorageBucket    string
	StaticFileServiceURL   string
	InternalAPIKey         string
	Audience               string
	Issuer                 string
	MacroAPITokenIssuer    string
	JWTSecretKey           string // resolved HMAC secret
	MacroAPITokenPublicKey string // resolved RSA public key (PEM)
	LocalAWSURL            string
	LocalAWSPublicURL      string
	S3EventQueue           string
	DisableEventPoll       bool // set via STATIC_FILE_SERVICE_DISABLE_EVENT_POLL=1 or LOCAL_AUTH
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("missing environment variable %s", name)
	}
	return v, nil
}

// Load mirrors Config::from_env plus JwtValidationArgs::new_with_secret_manager.
// In dev/prod the JWT_SECRET_KEY / MACRO_API_TOKEN_PUBLIC_KEY env vars hold a
// Secrets Manager secret name whose value is fetched at startup; locally the
// env var value is used directly.
func Load(ctx context.Context) (*Config, error) {
	env := EnvironmentFromEnv()

	tableName, err := requireEnv("STATIC_FILE_SERVICE_DYNAMODB_TABLE_NAME")
	if err != nil {
		return nil, err
	}
	bucket, err := requireEnv("STATIC_STORAGE_BUCKET")
	if err != nil {
		return nil, err
	}
	internalAPIKey, err := requireEnv("INTERNAL_API_KEY")
	if err != nil {
		return nil, err
	}
	audience, err := requireEnv("AUDIENCE")
	if err != nil {
		return nil, err
	}
	issuer, err := requireEnv("ISSUER")
	if err != nil {
		return nil, err
	}
	apiTokenIssuer, err := requireEnv("MACRO_API_TOKEN_ISSUER")
	if err != nil {
		return nil, err
	}
	jwtSecretVar, err := requireEnv("JWT_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	apiTokenPubKeyVar, err := requireEnv("MACRO_API_TOKEN_PUBLIC_KEY")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Environment:          env,
		Port:                 8080,
		DynamoDBTableName:    tableName,
		StaticStorageBucket:  bucket,
		StaticFileServiceURL: staticFileServiceURL(env),
		InternalAPIKey:       internalAPIKey,
		Audience:             audience,
		Issuer:               issuer,
		MacroAPITokenIssuer:  apiTokenIssuer,
		LocalAWSURL:          os.Getenv("LOCAL_AWS_URL"),
		LocalAWSPublicURL:    os.Getenv("LOCAL_AWS_PUBLIC_URL"),
		S3EventQueue:         s3EventQueue(env),
		DisableEventPoll:     os.Getenv("LOCAL_AUTH") != "" || os.Getenv("STATIC_FILE_SERVICE_DISABLE_EVENT_POLL") != "",
	}
	if v := os.Getenv("PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			cfg.Port = p
		}
	}

	awsCfg, err := LoadAWSConfig(ctx, cfg.LocalAWSURL)
	if err != nil {
		return nil, err
	}
	cfg.JWTSecretKey, err = resolveMaybeSecret(ctx, env, awsCfg, jwtSecretVar)
	if err != nil {
		return nil, fmt.Errorf("JWT_SECRET_KEY: %w", err)
	}
	cfg.MacroAPITokenPublicKey, err = resolveMaybeSecret(ctx, env, awsCfg, apiTokenPubKeyVar)
	if err != nil {
		return nil, fmt.Errorf("MACRO_API_TOKEN_PUBLIC_KEY: %w", err)
	}
	return cfg, nil
}

// resolveMaybeSecret mirrors SecretManager::get_maybe_secret_value: locally the
// env var value is the secret itself; in dev/prod it names a Secrets Manager
// secret to fetch.
func resolveMaybeSecret(ctx context.Context, env Environment, awsCfg aws.Config, value string) (string, error) {
	if env == EnvLocal {
		return value, nil
	}
	client := secretsmanager.NewFromConfig(awsCfg)
	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(value),
	})
	if err != nil {
		return "", fmt.Errorf("secretsmanager GetSecretValue %q: %w", value, err)
	}
	if out.SecretString == nil {
		return "", errors.New("secret has no string value")
	}
	return *out.SecretString, nil
}

// LoadAWSConfig mirrors macro_aws_config::get_macro_aws_config: when
// LOCAL_AWS_URL is set, build a LocalStack-pointed config with test
// credentials; otherwise the default chain pinned to us-east-1.
func LoadAWSConfig(ctx context.Context, localAWSURL string) (aws.Config, error) {
	if localAWSURL != "" {
		return awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
			awsconfig.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
				func(service, region string, options ...interface{}) (aws.Endpoint, error) {
					return aws.Endpoint{URL: localAWSURL}, nil
				}),
			),
		)
	}
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
}
