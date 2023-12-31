package lib

import "time"

type RedisConfig struct {
	Host      string `mapstructure:"host" validate:"required"`
	Port      int    `mapstructure:"port" validate:"required"`
	Password  string `mapstructure:"password"`
	DB        int    `mapstructure:"db" validate:"required"`
	KeyPrefix string `mapstructure:"key_prefix"`
}

type Config struct {
	GitHubToken              string        `mapstructure:"github_token"`
	Repo                     string        `mapstructure:"repo" validate:"required"`
	SaveAssetsPath           string        `mapstructure:"save_assets_path" validate:"required"`
	GitHubAPIEndpoint        string        `mapstructure:"github_api"`
	DeployCommand            string        `mapstructure:"deploy_command"  validate:"required"`
	RollbackCommand          string        `mapstructure:"rollback_command"`
	HealthCheckCommand       string        `mapstructure:"healthcheck_command" validate:"required"`
	VersionCommand           string        `mapstructure:"version_command" validate:"required"`
	HealthCheckInterval      time.Duration `mapstructure:"healthcheck_interval" validate:"required"`
	CanaryRolloutWindow      time.Duration `mapstructure:"canary_rollout_window" validate:"required"`
	RolloutWindow            time.Duration `mapstructure:"rollout_window" validate:"required"`
	RepositryPollingInterval time.Duration `mapstructure:"repository_polling_interval" validate:"required"`
	PackageNamePattern       string        `mapstructure:"package_name_pattern" validate:"required"`
	SlackWebhookURL          string        `mapstructure:"slack_webhook_url"`
	SlackChannel             string        `mapstructure:"slack_channel"`
	Redis                    *RedisConfig  `mapstructure:"redis" validate:"required"`
	LogLevel                 string        `mapstructure:"log_level"`
	HealthCheckRetries       uint          `mapstructure:"healthcheck_retries" validate:"required"`
	HealthCheckTimeout       time.Duration `mapstructure:"healthcheck_timeout" validate:"required"`
	IncludePreRelease        bool          `mapstructure:"include_prerelease"`
}
