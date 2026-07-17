package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/spf13/cobra"
)

var platformNodeUpgradeContractBase64 string
var platformNodeUpgradeLeaseToken string

var platformNodeUpgradeGuardCmd = &cobra.Command{
	Use:           "upgrade-publication-guard",
	Short:         "Verify protected node state before binary publication",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		if !platform.RunningAsRoot() {
			return fmt.Errorf("node upgrade publication guard must run as root")
		}
		var contract *nodeidentity.UpgradeContract
		if platformNodeUpgradeContractBase64 != "" {
			data, err := base64.StdEncoding.DecodeString(platformNodeUpgradeContractBase64)
			if err != nil {
				return fmt.Errorf("decode node upgrade contract: %w", err)
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			contract = &nodeidentity.UpgradeContract{}
			if err := decoder.Decode(contract); err != nil {
				return fmt.Errorf("decode node upgrade contract: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				return fmt.Errorf("node upgrade contract must contain exactly one JSON value")
			}
		}
		return provisioner.PublishTakodUpgrade(contract, platformNodeUpgradeLeaseToken)
	},
}

func init() {
	platformNodeCmd.AddCommand(platformNodeUpgradeGuardCmd)
	platformNodeUpgradeGuardCmd.Flags().StringVar(&platformNodeUpgradeContractBase64, "contract-base64", "", "Exact protected node contract")
	platformNodeUpgradeGuardCmd.Flags().StringVar(&platformNodeUpgradeLeaseToken, "lease-token", "", "Owning node upgrade lease token")
	_ = platformNodeUpgradeGuardCmd.MarkFlagRequired("lease-token")
}
