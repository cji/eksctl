package utils

import (
	"fmt"
	"strings"

	"github.com/kris-nova/logger"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/util/sets"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/ctl/cmdutils"
	"github.com/weaveworks/eksctl/pkg/eks"
	"github.com/weaveworks/eksctl/pkg/printers"
)

func enableLoggingCmd(rc *cmdutils.ResourceCmd) {
	cfg := api.NewClusterConfig()
	rc.ClusterConfig = cfg

	rc.SetDescription("enable-logging", "Update cluster logging configuration", "")

	rc.SetRunFuncWithNameArg(func() error {
		return doEnableLogging(rc)
	})

	rc.FlagSetGroup.InFlagSet("General", func(fs *pflag.FlagSet) {
		cmdutils.AddNameFlag(fs, cfg.Metadata)
		cmdutils.AddRegionFlag(fs, rc.ProviderConfig)
		cmdutils.AddConfigFileFlag(fs, &rc.ClusterConfigFile)
		cmdutils.AddApproveFlag(fs, rc)
	})

	rc.FlagSetGroup.InFlagSet("Logging Facilities", func(fs *pflag.FlagSet) {
		allSupportedLogTypes := api.SupportedCloudWatchClusterLoggingTypes()

		enableAll := fs.Bool("all", true,
			fmt.Sprintf("Enable all supported log types (%s)", strings.Join(allSupportedLogTypes, ", ")))
		for _, logType := range allSupportedLogTypes {
			_ = fs.Bool(logType, false, fmt.Sprintf("Enable %q log type", logType))
		}

		cmdutils.AddPreRun(rc.Command, func(cmd *cobra.Command, args []string) {
			disabled := sets.NewString()
			for _, logType := range allSupportedLogTypes {
				f := cmd.Flag(logType)

				shouldEnable := f.Value.String() == "true"
				shouldDisable := f.Changed && !shouldEnable

				if shouldEnable {
					cfg.CloudWatch.ClusterLogging.EnableTypes = append(cfg.CloudWatch.ClusterLogging.EnableTypes, logType)
					if !cmd.Flag("all").Changed {
						*enableAll = false
					}
				}

				if shouldDisable {
					disabled.Insert(logType)
				}
			}
			if *enableAll {
				for _, logType := range allSupportedLogTypes {
					if !disabled.Has(logType) {
						cfg.CloudWatch.ClusterLogging.EnableTypes = append(cfg.CloudWatch.ClusterLogging.EnableTypes, logType)
					}
				}
			}
		})
	})

	cmdutils.AddCommonFlagsForAWS(rc.FlagSetGroup, rc.ProviderConfig, false)
}

func doEnableLogging(rc *cmdutils.ResourceCmd) error {
	if err := cmdutils.NewMetadataLoader(rc).Load(); err != nil { // TODO: need a special loader here
		return err
	}

	cfg := rc.ClusterConfig
	meta := rc.ClusterConfig.Metadata

	if err := api.SetClusterConfigDefaults(cfg); err != nil {
		return err
	}

	printer := printers.NewJSONPrinter()
	ctl := eks.New(rc.ProviderConfig, cfg)

	if !ctl.IsSupportedRegion() {
		return cmdutils.ErrUnsupportedRegion(rc.ProviderConfig)
	}
	logger.Info("using region %s", meta.Region)

	if err := ctl.CheckAuth(); err != nil {
		return err
	}

	currentlyEnabled, _, err := ctl.GetCurrentClusterConfigForLogging(meta)
	if err != nil {
		return err
	}

	shouldEnable := sets.NewString()

	if cfg.CloudWatch != nil && cfg.CloudWatch.ClusterLogging != nil {
		shouldEnable.Insert(cfg.CloudWatch.ClusterLogging.EnableTypes...)
	}

	shouldDisable := sets.NewString(api.SupportedCloudWatchClusterLoggingTypes()...).Difference(shouldEnable)

	updateRequired := !currentlyEnabled.Equal(shouldEnable)

	if err := printer.LogObj(logger.Debug, "cfg.json = \\\n%s\n", cfg); err != nil {
		return err
	}

	if updateRequired {
		describeTypesToEnable := "no types to enable"
		if len(shouldEnable.List()) > 0 {
			describeTypesToEnable = fmt.Sprintf("enable types: %s", strings.Join(shouldEnable.List(), ", "))
		}

		describeTypesToDisable := "no types to disable"
		if len(shouldDisable.List()) > 0 {
			describeTypesToDisable = fmt.Sprintf("disable types: %s", strings.Join(shouldDisable.List(), ", "))
		}

		cmdutils.LogIntendedAction(rc.Plan, "update CloudWatch logging for cluster %q in %q (%s & %s)",
			meta.Name, meta.Region, describeTypesToEnable, describeTypesToDisable,
		)
		if !rc.Plan {
			if err := ctl.UpdateClusterConfigForLogging(cfg); err != nil {
				return err
			}
		}
	} else {
		logger.Success("CloudWatch logging for cluster %q in %q is already up-to-date", meta.Name, meta.Region)
	}

	cmdutils.LogPlanModeWarning(rc.Plan && updateRequired)

	return nil
}
