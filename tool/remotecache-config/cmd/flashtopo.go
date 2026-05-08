package cmd

import (
	"fmt"
	"strings"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/spf13/cobra"
)

func newFlashTopoCmd(client *master.MasterClient) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flashtopo [COMMAND]",
		Short: "flash topology management",
	}
	cmd.AddCommand(
		newCmdFlashTopoList(client),
		newCmdFlashTopoAdd(client),
		newCmdFlashTopoDel(client),
		newCmdFlashTopoRename(client),
		newCmdFlashTopoUpdate(client),
	)
	return cmd
}

func newCmdFlashTopoList(client *master.MasterClient) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list all flash topologies",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ftvs, err := client.AdminAPI().ListAllFlashTopos()
			if err != nil {
				return
			}
			stdoutln("Flash Topologies:")
			stdoutln(formatFlashTopoViews(ftvs))
			return
		},
	}
}

func formatFlashTopoViews(ftvs []*proto.FlashTopologyAdminView) string {
	var sb strings.Builder
	for i, ftv := range ftvs {
		if ftv == nil {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("  [%d]\n", i))
		sb.WriteString(fmt.Sprintf("    ID              : %d\n", ftv.ID))
		sb.WriteString(fmt.Sprintf("    Name            : %s\n", ftv.Name))
		sb.WriteString(fmt.Sprintf("    Region          : %s\n", ftv.Region))
		sb.WriteString(fmt.Sprintf("    Status          : %s\n", ftv.Status))
		sb.WriteString(fmt.Sprintf("    DelayDeleteTime : %s\n", ftv.DelayDeleteTime))
		sb.WriteString(fmt.Sprintf("    HandleReadTimeout   : %d\n", ftv.FlashNodeHandleReadTimeout))
		sb.WriteString(fmt.Sprintf("    ReadDataNodeTimeout : %d\n", ftv.FlashNodeReadDataNodeTimeout))
		sb.WriteString(fmt.Sprintf("    HotKeyMissCount     : %d\n", ftv.FlashHotKeyMissCount))
		sb.WriteString(fmt.Sprintf("    ReadFlowLimit       : %d\n", ftv.FlashReadFlowLimit))
		sb.WriteString(fmt.Sprintf("    WriteFlowLimit      : %d\n", ftv.FlashWriteFlowLimit))
		sb.WriteString(fmt.Sprintf("    KeyFlowLimit        : %d\n", ftv.FlashKeyFlowLimit))
	}
	return sb.String()
}

func newCmdFlashTopoUpdate(client *master.MasterClient) *cobra.Command {
	var (
		name                         string
		optYes                       bool
		flashNodeHandleReadTimeout   int
		flashNodeReadDataNodeTimeout int
		flashHotKeyMissCount         int
		flashReadFlowLimit           int64
		flashWriteFlowLimit          int64
		flashKeyFlowLimit            int64
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "update flash topology config",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			var topo *proto.FlashTopologyAdminView
			if name == "" {
				name = proto.DefaultTopoName
			}
			if topo, err = getFlashTopoView(client, name); err != nil {
				return
			}
			isChange := false
			confirmString := strings.Builder{}
			confirmString.WriteString("Flash topology configuration changes:\n")
			confirmString.WriteString(fmt.Sprintf("  Name                         : %s\n", topo.Name))
			appendFlashTopoIntChange(&confirmString, "FlashNodeHandleReadTimeout", topo.FlashNodeHandleReadTimeout, flashNodeHandleReadTimeout, &isChange)
			appendFlashTopoIntChange(&confirmString, "FlashNodeReadDataNodeTimeout", topo.FlashNodeReadDataNodeTimeout, flashNodeReadDataNodeTimeout, &isChange)
			appendFlashTopoIntChange(&confirmString, "FlashHotKeyMissCount", topo.FlashHotKeyMissCount, flashHotKeyMissCount, &isChange)
			appendFlashTopoInt64Change(&confirmString, "FlashReadFlowLimit", topo.FlashReadFlowLimit, flashReadFlowLimit, &isChange)
			appendFlashTopoInt64Change(&confirmString, "FlashWriteFlowLimit", topo.FlashWriteFlowLimit, flashWriteFlowLimit, &isChange)
			appendFlashTopoInt64Change(&confirmString, "FlashKeyFlowLimit", topo.FlashKeyFlowLimit, flashKeyFlowLimit, &isChange)
			if !isChange {
				stdout("No changes has been set.\n")
				return
			}
			if !optYes {
				stdout("%v", confirmString.String())
				stdout("\nConfirm (yes/no)[yes]: ")
				var userConfirm string
				_, _ = fmt.Scanln(&userConfirm)
				if userConfirm != "yes" && len(userConfirm) != 0 {
					return fmt.Errorf("Abort by user.\n")
				}
			}
			_, err = client.AdminAPI().UpdateFlashTopo(name, flashNodeHandleReadTimeout,
				flashNodeReadDataNodeTimeout, flashHotKeyMissCount, flashReadFlowLimit, flashWriteFlowLimit, flashKeyFlowLimit)
			if err != nil {
				return
			}
			stdoutlnf("update flash topology %s success", name)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().BoolVarP(&optYes, "yes", "y", false, "Answer yes for all questions")
	cmd.Flags().IntVar(&flashNodeHandleReadTimeout, "flashNodeHandleReadTimeout", -1, "flash node handle read timeout")
	cmd.Flags().IntVar(&flashNodeReadDataNodeTimeout, "flashNodeReadDataNodeTimeout", -1, "flash node read datanode timeout")
	cmd.Flags().IntVar(&flashHotKeyMissCount, "flashHotKeyMissCount", -1, "flash hot key miss count")
	cmd.Flags().Int64Var(&flashReadFlowLimit, "flashReadFlowLimit", -1, "flash read flow limit")
	cmd.Flags().Int64Var(&flashWriteFlowLimit, "flashWriteFlowLimit", -1, "flash write flow limit")
	cmd.Flags().Int64Var(&flashKeyFlowLimit, "flashKeyFlowLimit", -1, "flash key flow limit")
	return cmd
}

func getFlashTopoView(client *master.MasterClient, name string) (*proto.FlashTopologyAdminView, error) {
	ftvs, err := client.AdminAPI().ListAllFlashTopos()
	if err != nil {
		return nil, err
	}
	for _, ftv := range ftvs {
		if ftv != nil && ftv.Name == name {
			return ftv, nil
		}
	}
	return nil, fmt.Errorf("topo[%v] is not exist", name)
}

func appendFlashTopoIntChange(sb *strings.Builder, label string, oldValue int, newValue int, isChange *bool) {
	if newValue >= 0 {
		if oldValue != newValue {
			*isChange = true
			sb.WriteString(fmt.Sprintf("  %-28s : %d -> %d\n", label, oldValue, newValue))
			return
		}
		sb.WriteString(fmt.Sprintf("  %-28s : %d\n", label, oldValue))
	}
}

func appendFlashTopoInt64Change(sb *strings.Builder, label string, oldValue int64, newValue int64, isChange *bool) {
	if newValue >= 0 {
		if oldValue != newValue {
			*isChange = true
			sb.WriteString(fmt.Sprintf("  %-28s : %d -> %d\n", label, oldValue, newValue))
			return
		}
		sb.WriteString(fmt.Sprintf("  %-28s : %d\n", label, oldValue))
	}
}

func newCmdFlashTopoAdd(client *master.MasterClient) *cobra.Command {
	var name string
	var region string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "add a flash topology",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if name == "" {
				name = proto.DefaultTopoName
			}
			if region == "" {
				region = proto.DefaultRegion
			}
			result, err := client.AdminAPI().AddFlashTopo(name, region)
			if err != nil {
				return
			}
			stdoutln(result)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().StringVar(&region, "region", proto.DefaultRegion, "flash topology region")
	return cmd
}

func newCmdFlashTopoDel(client *master.MasterClient) *cobra.Command {
	var name string
	var optYes bool
	var optGradualFlag bool
	var optStep uint32
	cmd := &cobra.Command{
		Use:   "del",
		Short: "delete a flash topology",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if name == "" {
				name = proto.DefaultTopoName
			}
			if !optYes {
				stdout("delete flash topology: %s\n", name)
				stdout("\nConfirm (yes/no)[no]: ")
				var userConfirm string
				_, _ = fmt.Scanln(&userConfirm)
				if userConfirm != "yes" {
					return fmt.Errorf("Abort by user.\n")
				}
			}
			if optGradualFlag && optStep == 0 {
				return fmt.Errorf("param step(%v) must greater than 0", optStep)
			}
			result, err := client.AdminAPI().DelFlashTopo(name, optGradualFlag, optStep, false)
			if err != nil {
				return
			}
			stdoutln(result)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().BoolVarP(&optYes, "yes", "y", false, "Answer yes for all questions")
	cmd.Flags().BoolVar(&optGradualFlag, "gradualFlag", false, "set whether the topology's slots are deleted gradually or not(default false)")
	cmd.Flags().Uint32Var(&optStep, "step", 1, "set the step size(default 1) for slot gradual deletion")
	return cmd
}

func newCmdFlashTopoRename(client *master.MasterClient) *cobra.Command {
	return &cobra.Command{
		Use:   "rename [srcName] [dstName]",
		Short: "rename a flash topology",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			srcName := strings.TrimSpace(args[0])
			dstName := strings.TrimSpace(args[1])
			if srcName == "" {
				return fmt.Errorf("srcName should not be empty")
			}
			if dstName == "" {
				return fmt.Errorf("dstName should not be empty")
			}
			result, err := client.AdminAPI().RenameFlashTopo(srcName, dstName)
			if err != nil {
				return
			}
			stdoutln(result)
			return
		},
	}
}
