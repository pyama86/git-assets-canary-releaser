/*
Copyright © 2023 pyama86 <www.kazu.com@gmail.com>
*/
package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/go-playground/validator/v10"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/pyama86/git-assets-canary-releaser/lib"
	slogmulti "github.com/samber/slog-multi"
	slogslack "github.com/samber/slog-slack/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "git-assets-canary-releaser",
	Short: "This command downloads release assets from GitHub and deploys them.",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		config, err := loadConfig()
		if err != nil {
			slog.Error(fmt.Sprintf("failed to load config: %s", err))
			os.Exit(1)
		}

		logger, err := getLogger(config, config.LogLevel)
		if err != nil {
			slog.Error(fmt.Sprintf("failed to init logger: %s", err))
			os.Exit(1)
		}
		slog.SetDefault(logger)

		if err := runServer(config); err != nil {
			slog.Error(fmt.Sprintf("failed to run server: %s", err))
			// wait for slack notification
			// https://github.com/samber/slog-slack/blob/main/handler.go#L89
			if config.SlackWebhookURL != "" {
				time.Sleep(3 * time.Second)
			}
			os.Exit(1)
		}
	},
}

func deploy(cmd, targetTag string, state *lib.State, github lib.GitHuber) (string, string, error) {
	tag, downloadFile, err := github.DownloadReleaseAsset(targetTag)
	if err != nil {
		return "", "", fmt.Errorf("can't get release asset:%s %s", tag, err)
	}

	currentVersion, err := state.GetLastInstalledTag()
	if err != nil {
		return "", "", fmt.Errorf("can't get current version:%s", err)
	}

	slog.Info("deploy version info", slog.String("current_version", currentVersion), slog.String("new_version", tag))

	out, err := executeCommand(cmd, tag, downloadFile, 5*time.Minute)
	if err != nil {
		return "", "", fmt.Errorf("failed to execute command: %s, %s", err, out)
	}
	return tag, downloadFile, nil
}

func handleRollout(config *lib.Config, github lib.GitHuber, state *lib.State) error {
	if err := state.SaveMemberState(); err != nil {
		return err
	}

	tag, err := state.CurrentStableTag()
	if err != nil {
		return err
	}

	if tag == "" {
		return nil
	}

	if err := state.CanInstallTag(tag); err != nil {
		return err
	}
	got, err := state.TryRolloutLock(tag)
	if err != nil {
		return err
	}
	if got {
		slog.Info("lock success and start rollout", "tag", tag)
		if _, _, err := deploy(config.DeployCommand, tag, state, github); err != nil {
			return errors.Wrap(err, "deploy command failed")
		}

		if err := state.SaveMemberState(); err != nil {
			slog.Error(fmt.Sprintf("failed to save state: %s", err))
		}

		installed, all, err := state.GetRolloutProgress(tag)
		if err != nil {
			return err
		}
		slog.Info("rollout success", "tag", tag, "progress", fmt.Sprintf("%d/%d", installed, all))
	}
	return nil
}

func handleCanaryRelease(config *lib.Config, github lib.GitHuber, state *lib.State) error {
	if err := state.SaveMemberState(); err != nil {
		return err
	}

	// ロールバックのためにインストール前にインストール前のバージョンを取得しておく
	lastInstalledTag, err := state.GetLastInstalledTag()
	if err != nil {
		return err
	}

	stableTab, err := state.CurrentStableTag()
	if err != nil {
		return err
	}

	tag, _, err := github.DownloadReleaseAsset(lib.LatestTag)
	if err != nil {
		return fmt.Errorf("can't get release asset:%s %s", tag, err)
	}

	if tag == stableTab {
		return nil
	}

	err = state.CanInstallTag(tag)
	if err != nil {
		return err
	}

	got, err := state.TryCanaryReleaseLock(tag)
	if err != nil {
		return err
	}

	if got {
		slog.Info("lock success and start canary release", "tag", tag)
		if tag, filename, err := deploy(config.DeployCommand, tag, state, github); err != nil {
			return errors.Wrap(err, "deploy command failed")
		} else {
			slog.Info("deploy command success and start health check", "tag", tag, "cmd", config.HealthCheckCommand)
			if out, err := runHealthCheck(config, tag, filename); err != nil {
				slog.Error("health check command failed", slog.String("err", err.Error()), slog.String("out", out))
				if err := state.SaveAvoidReleaseTag(tag); err != nil {
					return fmt.Errorf("can't save avoid tag:%s", err)
				}

				// try rollback
				rollbackTag, err := state.RollbackTag(lastInstalledTag)
				if err != nil {
					return err
				}
				return handleRollback(rollbackTag, config, state, github)
			} else {
				slog.Info("health check success", "tag", tag)
				if err := state.SaveStableReleaseTag(tag); err != nil {
					return fmt.Errorf("can't save stable tag:%s", err)
				}

				if err := state.SaveMemberState(); err != nil {
					slog.Error(fmt.Sprintf("failed to save state: %s", err))
				}

				if err := state.UnlockCanaryRelease(); err != nil {
					return fmt.Errorf("can't unlock canary release tag")
				}
				slog.Info("canary release success", "tag", tag)
				return nil
			}
		}
	}
	return nil
}

var ErrRollback = errors.New("rollback")
var ErrNoRollback = errors.New("no rollback")

func handleRollback(rollbackTag string, config *lib.Config, state *lib.State, github lib.GitHuber) error {
	if config.RollbackCommand == "" {
		return ErrNoRollback
	}
	slog.Info("start rollback", "tag", rollbackTag)
	if _, _, err := deploy(config.RollbackCommand, rollbackTag, state, github); err != nil {
		return errors.Wrap(err, "rollback command failed")
	}
	slog.Info("rollback success", "tag", rollbackTag)
	return ErrRollback

}
func runServer(config *lib.Config) error {
	github, err := lib.NewGitHub(config)
	if err != nil {
		return err
	}

	gitTicker := time.NewTicker(config.RepositryPollingInterval)
	if viper.GetBool("once") {
		gitTicker = time.NewTicker(time.Nanosecond)
	}
	defer gitTicker.Stop()

	rolloutTicker := time.NewTicker(config.RolloutWindow)
	if viper.GetBool("once") {
		rolloutTicker = time.NewTicker(time.Nanosecond)
	}
	defer rolloutTicker.Stop()

	state, err := lib.NewState(config)
	if err != nil {
		return err
	}

	for {
		select {
		case <-rolloutTicker.C:
			if err := handleRollout(config, github, state); err != nil {
				if errors.Is(err, lib.ErrAlreadyInstalled) {
					slog.Debug("can't rollout", "err", err)
				} else if errors.Is(err, lib.ErrAssetsCannotDownload) {
					slog.Warn("can't get assets files")
				} else {
					return err
				}
			}
			if viper.GetBool("once") {
				rolloutTicker.Stop()
			}
		case <-gitTicker.C:
			if err := handleCanaryRelease(config, github, state); err != nil {
				if errors.Is(err, lib.ErrAssetsNotFound) ||
					errors.Is(err, lib.ErrAlreadyInstalled) ||
					errors.Is(err, lib.ErrAvoidReleaseTag) {
					slog.Debug("can't rollout", "err", err)
				} else if errors.Is(err, lib.ErrAssetsCannotDownload) {
					slog.Warn("can't get assets files")
				} else {
					if errors.Is(err, ErrRollback) {
						slog.Warn("rollback success")
					} else if errors.Is(err, ErrNoRollback) {
						slog.Info("no rollback because no rollback command")
					} else {
						return err
					}
				}

			}
			if viper.GetBool("once") {
				return nil
			}
		}
	}
}

func runHealthCheck(config *lib.Config, tag, file string) (string, error) {
	healthCheckTick := time.NewTicker(config.HealthCheckInterval)
	canaryReleaseTick := time.NewTicker(config.CanaryRolloutWindow)

	if viper.GetBool("once") {
		healthCheckTick = time.NewTicker(time.Nanosecond)
		canaryReleaseTick = time.NewTicker(time.Nanosecond)
	}
	defer healthCheckTick.Stop()
	defer canaryReleaseTick.Stop()
	f := func() (string, error) {
		ret := ""
		cxt, cancel := context.WithTimeout(
			context.Background(),
			config.HealthCheckTimeout*time.Duration(config.HealthCheckRetries)+
				config.HealthCheckInterval*time.Duration(config.HealthCheckRetries))
		defer cancel()
		err := retry.Do(
			func() error {
				out, err := executeCommand(config.HealthCheckCommand, tag, file, config.HealthCheckTimeout)
				ret = string(out)
				if err != nil {
					return fmt.Errorf("health check command failed: %s, %s", err.Error(), string(out))
				}
				return nil
			},
			retry.Context(cxt),
			retry.Attempts(config.HealthCheckRetries),
			retry.Delay(config.HealthCheckInterval),
		)
		return ret, err
	}
	if out, err := f(); err != nil {
		return out, err
	}

	for {
		select {
		case <-healthCheckTick.C:
			if out, err := f(); err != nil {
				return out, err
			}

		case <-canaryReleaseTick.C:
			return "", nil
		}
	}
}

func executeCommand(command string, tag, file string, timeout time.Duration) ([]byte, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), fmt.Sprintf("RELEASE_TAG=%s", tag))
	cmd.Env = append(cmd.Env, fmt.Sprintf("ASSET_FILE=%s", file))

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}

	slog.Debug("command result", "command", command, "out", string(out))
	return out, nil
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func getLogger(config *lib.Config, level string) (*slog.Logger, error) {
	logLevel := slog.LevelInfo
	var logOutput io.Writer
	switch level {
	case "info":
		logLevel = slog.LevelInfo
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level: %s", level)
	}
	ops := slog.HandlerOptions{
		Level: logLevel,
	}
	logOutput = os.Stdout
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %s", err)
	}

	logger := slog.New(slog.NewJSONHandler(logOutput, &ops)).With("host", hostname)
	if config.SlackWebhookURL != "" {
		logger = slog.New(
			slogmulti.Fanout(
				slog.NewJSONHandler(logOutput, &ops),
				slogslack.Option{
					Level:      logLevel,
					WebhookURL: config.SlackWebhookURL,
					Channel:    config.SlackChannel,
				}.NewSlackHandler(),
			),
		).With("host", hostname)
	}

	return logger, nil
}

func loadConfig() (*lib.Config, error) {
	p, err := homedir.Expand(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get config path: %s", err)
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("failed to get config path: %s", err)
	}
	viper.SetConfigType("toml")
	viper.SetEnvPrefix("GACR")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if _, err := os.Stat(p); err == nil {
		c, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}

		if err != nil {
			return nil, fmt.Errorf("failed to read config: %s", err)
		}

		if err := viper.ReadConfig(bytes.NewReader(c)); err != nil {
			return nil, fmt.Errorf("failed to read config: %s", err)
		}
	} else {
		slog.Warn("config file not found", slog.String("path", p))
	}

	config := lib.Config{}
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %s", err)
	}

	validate := validator.New(validator.WithRequiredStructEnabled())
	err = validate.Struct(&config)
	if err != nil {
		return nil, fmt.Errorf("faileh to validate config: %s", err)
	}
	return &config, nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "~/gacr.conf", "config file (default is $HOME/gacr.conf)")

	rootCmd.PersistentFlags().String("repo", "", "GitHub repository name")
	viper.BindPFlag("repo", rootCmd.PersistentFlags().Lookup("repo"))

	rootCmd.PersistentFlags().String("github-token", "", "GitHub token")
	viper.BindPFlag("github_token", rootCmd.PersistentFlags().Lookup("github-token"))

	rootCmd.PersistentFlags().String("github-api", "https://api.github.com", "GitHub API endpoint")
	viper.BindPFlag("github_api", rootCmd.PersistentFlags().Lookup("github-api"))

	rootCmd.PersistentFlags().String("deploy-command", "", "Deploy command")
	viper.BindPFlag("deploy_command", rootCmd.PersistentFlags().Lookup("deploy-command"))

	rootCmd.PersistentFlags().String("rollback-command", "", "Rollback command")
	viper.BindPFlag("rollback_command", rootCmd.PersistentFlags().Lookup("rollback-command"))

	rootCmd.PersistentFlags().String("healthcheck-command", "", "HealthCheck command")
	viper.BindPFlag("healthcheck_command", rootCmd.PersistentFlags().Lookup("healthcheck-command"))

	rootCmd.PersistentFlags().String("version-command", "", "Version command")
	viper.BindPFlag("version_command", rootCmd.PersistentFlags().Lookup("version-command"))

	rootCmd.PersistentFlags().String("slack-webhook-url", "", "Slack webhook URL")
	viper.BindPFlag("slack_webhook_url", rootCmd.PersistentFlags().Lookup("slack-webhook-url"))

	rootCmd.PersistentFlags().String("slack-channel", "", "Slack channel")
	viper.BindPFlag("slack_channel", rootCmd.PersistentFlags().Lookup("slack-channel"))

	rootCmd.PersistentFlags().String("redis-host", "127.0.0.1", "Redis host")
	viper.BindPFlag("redis.host", rootCmd.PersistentFlags().Lookup("redis-host"))

	rootCmd.PersistentFlags().Int("redis-port", 6379, "Redis port")
	viper.BindPFlag("redis.port", rootCmd.PersistentFlags().Lookup("redis-port"))

	rootCmd.PersistentFlags().String("redis-password", "", "Redis password")
	viper.BindPFlag("redis.password", rootCmd.PersistentFlags().Lookup("redis-password"))

	rootCmd.PersistentFlags().Int("redis-db", 1, "Redis DB")
	viper.BindPFlag("redis.db", rootCmd.PersistentFlags().Lookup("redis-db"))

	rootCmd.PersistentFlags().String("redis-key-prefix", "", "Redis key prefix(default repo name)")
	viper.BindPFlag("redis.key_prefix", rootCmd.PersistentFlags().Lookup("redis-key-prefix"))

	rootCmd.PersistentFlags().String("package-name-pattern", "", "Package name pattern")
	viper.BindPFlag("package_name_pattern", rootCmd.PersistentFlags().Lookup("package-name-pattern"))

	rootCmd.PersistentFlags().String("log-level", "info", "Log level")
	viper.BindPFlag("log_level", rootCmd.PersistentFlags().Lookup("log-level"))

	rootCmd.PersistentFlags().String("save-assets-path", "/usr/local/src", "assets download path")
	viper.BindPFlag("save_assets_path", rootCmd.PersistentFlags().Lookup("save-assets-path"))

	rootCmd.PersistentFlags().Duration("canary-rollout-window", 5*time.Minute, "canary release rollout window")
	viper.BindPFlag("canary_rollout_window", rootCmd.PersistentFlags().Lookup("canary-rollout-window"))

	rootCmd.PersistentFlags().Duration("rollout-window", 1*time.Minute, "release rollout window")
	viper.BindPFlag("rollout_window", rootCmd.PersistentFlags().Lookup("rollout-window"))

	rootCmd.PersistentFlags().Duration("health-check-interval", 1*time.Minute, "health check interval")
	viper.BindPFlag("healthcheck_interval", rootCmd.PersistentFlags().Lookup("health-check-interval"))

	rootCmd.PersistentFlags().Duration("repository-polling-interval", 5*time.Minute, "repository polling interval")
	viper.BindPFlag("repository_polling_interval", rootCmd.PersistentFlags().Lookup("repository-polling-interval"))

	rootCmd.PersistentFlags().Bool("once", false, "one shot mode")
	viper.BindPFlag("once", rootCmd.PersistentFlags().Lookup("once"))

	rootCmd.PersistentFlags().Int("healthcheck-retries", 3, "retry count of health check")
	viper.BindPFlag("healthcheck_retries", rootCmd.PersistentFlags().Lookup("healthcheck-retries"))

	rootCmd.PersistentFlags().Duration("healthcheck-timeout", 30*time.Second, "timeout of health check")
	viper.BindPFlag("healthcheck_timeout", rootCmd.PersistentFlags().Lookup("healthcheck-timeout"))

	rootCmd.PersistentFlags().Bool("include-prerelease", false, "include prerelease")
	viper.BindPFlag("include_prerelease", rootCmd.PersistentFlags().Lookup("include-prerelease"))
}
