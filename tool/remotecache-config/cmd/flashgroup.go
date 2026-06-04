package cmd

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/spf13/cobra"
)

const _flashgroupID = " [FlashGroupID]"

type slotInfo struct {
	fgID uint64
	slot uint32
}

func newFlashGroupCmd(client *master.MasterClient) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flashgroup [COMMAND]",
		Short: "cluster flashgroup management",
	}
	cmd.AddCommand(
		newCmdFlashGroupTurn(client),
		newCmdFlashGroupCreate(client),
		newCmdFlashGroupSet(client),
		newCmdFlashGroupRemove(client),
		newCmdFlashGroupNodeAdd(client),
		newCmdFlashGroupNodeRemove(client),
		newCmdFlashGroupAddSlots(client),
		newCmdFlashGroupSuggestSlots(client),
		newCmdFlashGroupGet(client),
		newCmdFlashGroupList(client),
		newCmdFlashGroupClient(client),
		newCmdFlashGroupSearch(client),
		newCmdFlashGroupGraph(client),
	)
	return cmd
}

func newCmdFlashGroupTurn(client *master.MasterClient) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "turn [IsEnable]",
		Short: "turn flash group cache",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			enabled, err := strconv.ParseBool(args[0])
			if err != nil {
				return
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			result, err := client.AdminAPI().TurnFlashGroupByName(name, enabled)
			if err != nil {
				return
			}
			stdoutln(result)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	return cmd
}

func newCmdFlashGroupCreate(client *master.MasterClient) *cobra.Command {
	var optSlots string
	var optWeight int
	var optGradualFlag bool
	var optStep uint32
	var name string
	cmd := &cobra.Command{
		Use:   CliOpCreate,
		Short: "create a new flash group",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if optSlots != "" {
				numbers := strings.Split(optSlots, ",")
				for _, numStr := range numbers {
					_, err = strconv.Atoi(numStr)
					if err != nil {
						return
					}
				}
			}
			if optWeight <= 0 || optWeight > proto.FlashGroupMaxWeight {
				err = fmt.Errorf("param weight(%v) must greater than 0 and not greater than %v", optWeight, proto.FlashGroupMaxWeight)
				return
			}

			if optGradualFlag {
				if optStep <= 0 {
					err = fmt.Errorf("param step(%v) must greater than 0", optStep)
					return
				}
			}

			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().CreateFlashGroupByName(name, optSlots, optWeight, optGradualFlag, optStep)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().StringVar(&optSlots, "slots", "", "set group in which slots, --slots=slot1,slot2,...")
	cmd.Flags().IntVar(&optWeight, "weight", proto.FlashGroupDefaultWeight, "set group weight(default 1, must 1<=weight<=30), if it was specified slots count equal to 32*weight")
	cmd.Flags().BoolVar(&optGradualFlag, "gradualFlag", false, "set whether the group's slots are created gradually or not(default false)")
	cmd.Flags().Uint32Var(&optStep, "step", 1, "set the step size(default 1) for slot gradual creation")
	return cmd
}

func newCmdFlashGroupSet(client *master.MasterClient) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   CliOpSet + _flashgroupID + " [IsActive]",
		Short: "set flash group active or not",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			isActive, err := strconv.ParseBool(args[1])
			if err != nil {
				return
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().SetFlashGroupByName(name, flashGroupID, isActive)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	return cmd
}

func newCmdFlashGroupRemove(client *master.MasterClient) *cobra.Command {
	var optYes bool
	var optGradualFlag bool
	var optStep uint32
	var name string
	cmd := &cobra.Command{
		Use:   CliOpRemove + _flashgroupID,
		Short: "remove flash group by id",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			// ask user for confirm
			if !optYes {
				fmt.Printf("remove flash group by id[%d]\n", flashGroupID)
				stdout("\nConfirm (yes/no)[no]: ")
				var userConfirm string
				_, _ = fmt.Scanln(&userConfirm)
				if userConfirm != "yes" {
					err = fmt.Errorf("Abort by user.\n")
					return
				}
			}

			if optGradualFlag {
				if optStep <= 0 {
					err = fmt.Errorf("param step(%v) must greater than 0", optStep)
					return
				}
			}

			if name == "" {
				name = proto.DefaultTopoName
			}
			result, err := client.AdminAPI().RemoveFlashGroupByName(name, flashGroupID, optGradualFlag, optStep)
			if err != nil {
				return
			}
			stdoutln(result)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().BoolVarP(&optYes, "yes", "y", false, "Answer yes for all questions")
	cmd.Flags().BoolVar(&optGradualFlag, "gradualFlag", false, "set whether the group's slots are deleted gradually or not(default false)")
	cmd.Flags().Uint32Var(&optStep, "step", 1, "set the step size(default 1) for slot gradual deletion")
	return cmd
}

func newCmdFlashGroupNodeAdd(client *master.MasterClient) *cobra.Command {
	var (
		optAddr     string
		optZoneName string
		optCount    int
		name        string
	)
	cmd := &cobra.Command{
		Use:   "nodeAdd" + _flashgroupID,
		Short: "add flash node to given flash group",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().FlashGroupAddFlashNodeByName(name, flashGroupID, optCount, optZoneName, optAddr)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().StringVar(&optAddr, CliFlagAddress, "", "add flash node of given addr")
	cmd.Flags().StringVar(&optZoneName, CliFlagFlashZoneName, "", "add flash node from given zone")
	cmd.Flags().IntVar(&optCount, CliFlagCount, 0, "add given count flash node from zone")
	return cmd
}

func newCmdFlashGroupNodeRemove(client *master.MasterClient) *cobra.Command {
	var (
		optAddr     string
		optZoneName string
		optCount    int
		optYes      bool
	)
	var name string
	cmd := &cobra.Command{
		Use:   "nodeRemove" + _flashgroupID,
		Short: "remove flash node to given flash group",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			// ask user for confirm
			if !optYes {
				fmt.Printf("remove flash node to given flash groupid[%d]\n", flashGroupID)
				if optAddr != "" {
					stdout("  FlashNode Addr   : %v\n", optAddr)
				} else {
					stdout("  Zone  : %v\n", optZoneName)
					stdout("  Count : %d\n", optCount)
				}
				stdout("\nConfirm (yes/no)[no]: ")
				var userConfirm string
				_, _ = fmt.Scanln(&userConfirm)
				if userConfirm != "yes" {
					err = fmt.Errorf("Abort by user.\n")
					return
				}
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().FlashGroupRemoveFlashNodeByName(name, flashGroupID, optCount, optZoneName, optAddr)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().StringVar(&optAddr, CliFlagAddress, "", "remove flash node of given addr")
	cmd.Flags().StringVar(&optZoneName, CliFlagFlashZoneName, "", "remove flash node from given zone")
	cmd.Flags().IntVar(&optCount, CliFlagCount, 0, "remove given count flash node from zone")
	cmd.Flags().BoolVarP(&optYes, "yes", "y", false, "Answer yes for all questions")
	return cmd
}

func newCmdFlashGroupAddSlots(client *master.MasterClient) *cobra.Command {
	var optSlots string
	var name string
	cmd := &cobra.Command{
		Use:   "addSlots" + _flashgroupID,
		Short: "add specified slots to a flash group",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			if optSlots == "" {
				err = fmt.Errorf("param slots is required")
				return
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().FlashGroupAddSlotsByName(name, flashGroupID, optSlots)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().StringVar(&optSlots, "slots", "", "slots to add, e.g., --slots=1,2,3")
	return cmd
}

func newCmdFlashGroupSuggestSlots(client *master.MasterClient) *cobra.Command {
	var optCount int
	var optErrorRate float64
	var name string
	cmd := &cobra.Command{
		Use:   "suggestSlots" + _flashgroupID,
		Short: "suggest slots to add for a given flash group to balance distribution",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			targetFgID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}

			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().ListFlashGroupsByName(name, false)
			if err != nil {
				return
			}

			var targetWeight uint32
			foundTarget := false
			for _, fg := range fgView.FlashGroups {
				if fg.ID == targetFgID {
					foundTarget = true
					targetWeight = fg.Weight
					break
				}
			}
			if !foundTarget {
				err = fmt.Errorf("target flash group %d not found", targetFgID)
				return
			}

			type slotRange struct {
				fgID    uint64
				slot    uint32
				start   uint32
				end     uint32
				percent float64
			}

			slots := make([]slotInfo, 0)
			activeFGs := 0
			for _, fg := range fgView.FlashGroups {
				if fg.Status == proto.FlashGroupStatus_Active {
					activeFGs++
					for _, slot := range fg.Slots {
						slots = append(slots, slotInfo{
							fgID: fg.ID,
							slot: slot,
						})
					}
				}
			}

			if activeFGs == 0 || len(slots) == 0 {
				stdoutln("No active flash groups or slots found to suggest from.")
				return nil
			}

			sort.Slice(slots, func(i, j int) bool {
				return slots[i].slot < slots[j].slot
			})

			fgTotalPercent := make(map[uint64]float64)
			var allRanges []slotRange

			n := len(slots)
			for i := 0; i < n; i++ {
				curr := slots[i]
				prev := slots[(i-1+n)%n]

				var dist uint64
				if i == 0 {
					dist = uint64(curr.slot) + (uint64(math.MaxUint32) - uint64(prev.slot)) + 1
				} else {
					dist = uint64(curr.slot) - uint64(prev.slot)
				}

				pct := float64(dist) * 100 / 4294967296.0
				startSlot := prev.slot + 1
				if i == 0 && prev.slot == math.MaxUint32 {
					startSlot = 0
				}

				sr := slotRange{
					fgID:    curr.fgID,
					slot:    curr.slot,
					start:   startSlot,
					end:     curr.slot,
					percent: pct,
				}
				allRanges = append(allRanges, sr)
				fgTotalPercent[curr.fgID] += pct
			}

			var totalWeight uint64
			expectedPct := make(map[uint64]float64)
			for _, fg := range fgView.FlashGroups {
				if fg.Status == proto.FlashGroupStatus_Active || fg.ID == targetFgID {
					totalWeight += uint64(fg.Weight)
				}
			}
			if totalWeight > 0 {
				for _, fg := range fgView.FlashGroups {
					if fg.Status == proto.FlashGroupStatus_Active || fg.ID == targetFgID {
						expectedPct[fg.ID] = float64(fg.Weight) * 100.0 / float64(totalWeight)
					}
				}
			}

			minTakePct := optErrorRate * 100

			var candidateRanges []slotRange
			for _, r := range allRanges {
				sourceThreshold := expectedPct[r.fgID] * (1.0 + optErrorRate)
				if fgTotalPercent[r.fgID] > sourceThreshold && r.fgID != targetFgID {
					candidateRanges = append(candidateRanges, r)
				}
			}

			if len(candidateRanges) == 0 {
				stdoutln("No suitable large flash groups found to take slots from.")
				return nil
			}

			sort.Slice(candidateRanges, func(i, j int) bool {
				return candidateRanges[i].percent > candidateRanges[j].percent
			})

			var suggestions []uint32
			tbl := table{{"From_FG", "Interval_Start", "Interval_End", "Original_Percent", "Taken_Percent", "Remain_Percent", "Source_Total_Percent", "Suggested_Slot"}}

			currentPct := fgTotalPercent[targetFgID]
			targetExpected := expectedPct[targetFgID]
			for i := 0; i < len(candidateRanges); i++ {
				if len(suggestions) >= optCount {
					break
				}
				neededPct := targetExpected - currentPct
				if neededPct <= minTakePct {
					break
				}

				r := candidateRanges[i]
				sourceThreshold := expectedPct[r.fgID] * (1.0 + optErrorRate)
				maxTakeFromSource := fgTotalPercent[r.fgID] - sourceThreshold
				if maxTakeFromSource <= minTakePct {
					continue
				}

				takePct := neededPct
				if takePct > maxTakeFromSource {
					takePct = maxTakeFromSource
				}
				if takePct > r.percent*0.5 {
					takePct = r.percent * 0.5
				}
				if takePct <= minTakePct {
					continue
				}

				var dist uint64
				if r.end < r.start {
					dist = uint64(r.end) + (uint64(math.MaxUint32) - uint64(r.start)) + 1
				} else {
					dist = uint64(r.end) - uint64(r.start) + 1
				}
				offset := uint64((takePct / 100.0) * 4294967296.0)
				if offset == 0 {
					offset = 1
				}
				if offset >= dist {
					offset = dist - 1
				}
				if offset == 0 {
					continue
				}

				suggestSlot := uint64(r.start) + offset - 1
				if suggestSlot > math.MaxUint32 {
					suggestSlot -= math.MaxUint32 + 1
				}
				suggestions = append(suggestions, uint32(suggestSlot))

				addedPct := float64(offset) * 100.0 / 4294967296.0
				currentPct += addedPct
				fgTotalPercent[r.fgID] -= addedPct
				remainPct := r.percent - addedPct

				tbl = tbl.append(arow(r.fgID, r.start, r.end, fmt.Sprintf("%0.5f%%", r.percent), fmt.Sprintf("%0.5f%%", addedPct), fmt.Sprintf("%0.5f%%", remainPct), fmt.Sprintf("%0.5f%%", fgTotalPercent[r.fgID]), uint32(suggestSlot)))
			}

			if len(suggestions) == 0 {
				stdoutln("Target FlashGroup already has enough percent, no slots needed to be added.")
				return nil
			}

			stdoutlnf("Target FlashGroup: %d, Weight: %v, Expected Percent: %0.5f%%", targetFgID, targetWeight, expectedPct[targetFgID])
			stdoutlnf("Percent: %0.5f%% -> %0.5f%%", fgTotalPercent[targetFgID], currentPct)
			stdoutln("\n[Suggested Slots to Add]")
			stdoutln(alignTable(tbl...))

			var strSlots []string
			for _, s := range suggestions {
				strSlots = append(strSlots, fmt.Sprintf("%d", s))
			}
			stdoutln("\nCommand to execute:")
			if name == proto.DefaultTopoName {
				stdoutlnf("./cfs-remotecache-config flashgroup addSlots %d --slots=%s", targetFgID, strings.Join(strSlots, ","))
			} else {
				stdoutlnf("./cfs-remotecache-config flashgroup addSlots %d --slots=%s -n %s", targetFgID, strings.Join(strSlots, ","), name)
			}

			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().IntVarP(&optCount, "count", "c", 8, "number of slots to suggest")
	cmd.Flags().Float64VarP(&optErrorRate, "errorRate", "e", 0.0001, "allowed error rate over average (e.g. 0.05 for 5% over avg)")
	return cmd
}

func newCmdFlashGroupGet(client *master.MasterClient) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   CliOpInfo + _flashgroupID + " [showHitRate ture/false] ",
		Short: "get flash group by id, default don't show hit rate",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			flashGroupID, err := parseFlashGroupID(args[0])
			if err != nil {
				return
			}
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().GetFlashGroupByName(name, flashGroupID)
			if err != nil {
				return
			}
			stdoutln(formatFlashGroupView(&fgView))

			showHitRate := false
			if len(args) > 1 {
				showHitRate, _ = strconv.ParseBool(args[1])
			}

			stdoutln("[Flash Nodes]")
			var tbl table
			if showHitRate {
				tbl = table{formatFlashNodeViewTableTitle}
			} else {
				tbl = table{formatFlashNodeSimpleViewTableTitle}
			}
			for _, flashNodeViewInfos := range fgView.ZoneFlashNodes {
				tbl = showFlashNodesView(flashNodeViewInfos, showHitRate, nil, tbl)
			}
			stdoutln(alignTable(tbl...))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	return cmd
}

func newCmdFlashGroupList(client *master.MasterClient) *cobra.Command {
	var name string
	var showAllTopo bool
	cmd := &cobra.Command{
		Use:   CliOpList + " [IsActive]",
		Short: "list active or inactive flash groups",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			var fgView proto.FlashGroupsAdminView
			var isActive bool
			if len(args) > 0 {
				if isActive, err = strconv.ParseBool(args[0]); err != nil {
					return
				}
				if name == "" {
					name = proto.DefaultTopoName
				}
				fgView, err = client.AdminAPI().ListFlashGroupByName(name, isActive, showAllTopo)
			} else {
				if name == "" {
					name = proto.DefaultTopoName
				}
				fgView, err = client.AdminAPI().ListFlashGroupsByName(name, showAllTopo)
			}
			if err != nil {
				return
			}
			stdoutln("[Flash Groups]")
			slots := make([]*slotInfo, 0)
			reservedSlots := make([]*slotInfo, 0)
			tbl := table{formatFlashGroupViewTile}
			for _, group := range fgView.FlashGroups {
				sort.Slice(group.Slots, func(i, j int) bool {
					return group.Slots[i] < group.Slots[j]
				})
				for _, slot := range group.Slots {
					slots = append(slots, &slotInfo{
						fgID: group.ID,
						slot: slot,
					})
				}
				sort.Slice(group.ReservedSlots, func(i, j int) bool {
					return group.ReservedSlots[i] < group.ReservedSlots[j]
				})
				for _, slot := range group.ReservedSlots {
					reservedSlots = append(reservedSlots, &slotInfo{
						fgID: group.ID,
						slot: slot,
					})
				}
				tbl = tbl.append(arow(group.ID, group.Weight, len(group.Slots), len(group.ReservedSlots), group.Status, group.SlotStatus, len(group.PendingSlots), group.Step, group.FlashNodeCount, group.IsReducingSlots, group.FlashNodeTopoName, group.Region))
			}
			stdoutln(alignTable(tbl...))

			printGroupedSlots := func(title string, sl []*slotInfo) {
				if len(sl) == 0 {
					return
				}
				stdoutln(title + ":")
				type slotRange struct {
					slot    uint32
					start   uint32
					end     uint32
					percent float64
				}
				fgSlots := make(map[uint64][]slotRange)
				fgTotalPercent := make(map[uint64]float64)
				n := len(sl)
				for i := 0; i < n; i++ {
					curr := sl[i]
					prev := sl[(i-1+n)%n]

					var dist uint64
					if i == 0 {
						dist = uint64(curr.slot) + (uint64(math.MaxUint32) - uint64(prev.slot)) + 1
					} else {
						dist = uint64(curr.slot) - uint64(prev.slot)
					}

					pct := float64(dist) * 100 / 4294967296.0
					startSlot := prev.slot + 1
					if i == 0 && prev.slot == math.MaxUint32 {
						startSlot = 0
					}

					fgSlots[curr.fgID] = append(fgSlots[curr.fgID], slotRange{
						slot:    curr.slot,
						start:   startSlot,
						end:     curr.slot,
						percent: pct,
					})
					fgTotalPercent[curr.fgID] += pct
				}

				var totalWeight uint64
				for _, fg := range fgView.FlashGroups {
					totalWeight += uint64(fg.Weight)
				}

				for _, fg := range fgView.FlashGroups {
					if len(fgSlots[fg.ID]) == 0 {
						continue
					}
					var expectedPct float64
					if totalWeight > 0 {
						expectedPct = float64(fg.Weight) * 100.0 / float64(totalWeight)
					}
					stdoutlnf("FlashGroup: %d, Weight: %v, Expected Percent: %0.5f%%, Total Percent: %0.5f%%", fg.ID, fg.Weight, expectedPct, fgTotalPercent[fg.ID])
					for _, sr := range fgSlots[fg.ID] {
						stdoutlnf("  slot:%d range:[%d, %d] percent:%0.5f%%", sr.slot, sr.start, sr.end, sr.percent)
					}
				}
			}

			sort.Slice(slots, func(i, j int) bool {
				return slots[i].slot < slots[j].slot
			})
			printGroupedSlots("Slots", slots)

			sort.Slice(reservedSlots, func(i, j int) bool {
				return reservedSlots[i].slot < reservedSlots[j].slot
			})
			printGroupedSlots("ReservedSlots", reservedSlots)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().BoolVar(&showAllTopo, "showAllTopo", false, "list flash groups across all topologies (default false)")
	return cmd
}

func newCmdFlashGroupClient(client *master.MasterClient) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "client",
		Short: "show flash group response passed from master to client",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgv, err := client.AdminAPI().ClientFlashGroups(name)
			if err != nil {
				return
			}
			stdoutln("Flash Group Response:")
			stdoutln(formatIndent(fgv))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", proto.DefaultTopoName, "flash topology name")
	return cmd
}

func newCmdFlashGroupSearch(client *master.MasterClient) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "search [volume] [inode] [offset]",
		Short: "search flash group by volume inode offset",
		Args:  cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			volume := args[0]
			if volume == "" {
				err = fmt.Errorf("volume is empty")
				return
			}
			inode, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				return
			}
			offset, err := strconv.ParseUint(args[2], 10, 64)
			if err != nil {
				return
			}
			slotKey := proto.ComputeCacheBlockSlot(volume, inode, offset)

			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().ListFlashGroupsByName(name, false)
			if err != nil {
				return
			}
			set := make(map[uint32]struct{})
			slots := make([]slotInfo, 0)
			for _, fg := range fgView.FlashGroups {
				if fg.Status != proto.FlashGroupStatus_Active {
					continue
				}
				for _, slot := range fg.Slots {
					if _, in := set[slot]; in {
						continue
					}
					slots = append(slots, slotInfo{
						fgID: fg.ID,
						slot: slot,
					})
				}
			}
			sort.Slice(slots, func(i, j int) bool {
				return slots[i].slot < slots[j].slot
			})

			var whichGroup uint64
			for _, slot := range slots {
				if slotKey >= slot.slot {
					whichGroup = slot.fgID
				}
			}
			for _, fg := range fgView.FlashGroups {
				if fg.ID == whichGroup {
					stdoutlnf("Found in FlashGroup:%d", whichGroup)
					tbl := table{formatFlashNodeSimpleViewTableTitle}
					for _, fnNodes := range fg.ZoneFlashNodes {
						tbl = showFlashNodesView(fnNodes, false, nil, tbl)
					}
					stdoutln(alignTable(tbl...))
					return
				}
			}
			stdoutlnf("Not found (%s %d %d) -> %d", volume, inode, offset, slotKey)
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	return cmd
}

func newCmdFlashGroupGraph(client *master.MasterClient) *cobra.Command {
	var name string
	var showAllTopo bool
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "show flash group and node",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if name == "" {
				name = proto.DefaultTopoName
			}
			fgView, err := client.AdminAPI().ListFlashGroupsByName(name, showAllTopo)
			if err != nil {
				return
			}
			set := make(map[uint32]struct{})
			groups := make(map[uint64]proto.FlashGroupAdminView)
			groupn := make(map[uint64]int)
			groupStatusMap := make(map[uint64]string)
			slots := make([]slotInfo, 0)
			reservedSlots := make([]slotInfo, 0)
			for _, fg := range fgView.FlashGroups {
				groups[fg.ID] = fg
				groupn[fg.ID] = 0
				groupStatusMap[fg.ID] = fg.Status.String()
				for _, slot := range fg.Slots {
					if _, in := set[slot]; in {
						continue
					}
					groupn[fg.ID]++
					slots = append(slots, slotInfo{
						fgID: fg.ID,
						slot: slot,
					})
				}
				for _, slot := range fg.ReservedSlots {
					if _, in := set[slot]; in {
						continue
					}
					groupn[fg.ID]++
					reservedSlots = append(reservedSlots, slotInfo{
						fgID: fg.ID,
						slot: slot,
					})
				}
			}
			sort.Slice(slots, func(i, j int) bool {
				return slots[i].slot < slots[j].slot
			})
			stdoutln("[Flash Groups]")
			stdoutln("[Slots]")
			tbl := table{arow("Slot", "ID", "Status", "Count", "Ref", "Proportion")}
			for idx, slot := range slots {
				g := groups[slot.fgID]
				var p string
				if idx == len(slots)-1 {
					p = proportion(slot.slot, math.MaxUint32)
				} else {
					p = proportion(slot.slot, slots[idx+1].slot)
				}
				tbl = tbl.append(arow(slot.slot, g.ID, g.Status.String(), g.FlashNodeCount, groupn[g.ID], p))
			}
			stdoutln(alignTable(tbl...))

			sort.Slice(reservedSlots, func(i, j int) bool {
				return reservedSlots[i].slot < reservedSlots[j].slot
			})
			stdoutln("[ReservedSlots]")
			tbl1 := table{arow("Slot", "ID", "Status", "Count", "Ref", "Proportion")}
			for idx, slot := range reservedSlots {
				g := groups[slot.fgID]
				var p string
				if idx == len(reservedSlots)-1 {
					p = proportion(slot.slot, math.MaxUint32)
				} else {
					p = proportion(slot.slot, reservedSlots[idx+1].slot)
				}
				tbl1 = tbl1.append(arow(slot.slot, g.ID, g.Status.String(), g.FlashNodeCount, groupn[g.ID], p))
			}

			stdoutln(alignTable(tbl1...))

			fnView, err := client.NodeAPI().ListFlashNodesByTopo(-1, name, showAllTopo)
			if err != nil {
				return
			}
			busyNodes := make([]*proto.FlashNodeViewInfo, 0)
			idleNodes := make([]*proto.FlashNodeViewInfo, 0)
			for _, nodes := range fnView {
				for _, node := range nodes {
					if node.FlashGroupID == 0 {
						idleNodes = append(idleNodes, node)
					} else {
						busyNodes = append(busyNodes, node)
					}
				}
			}
			graphFlashNodeTitle := make([]interface{}, len(formatFlashNodeViewTableTitle)+1)
			copy(graphFlashNodeTitle, formatFlashNodeViewTableTitle[:7])
			graphFlashNodeTitle[7] = "GroupStatus"
			copy(graphFlashNodeTitle[8:], formatFlashNodeViewTableTitle[7:])
			stdoutln("[FlashNodes Busy]")
			tbl = showFlashNodesView(busyNodes, true, groupStatusMap, table{graphFlashNodeTitle})
			stdoutln(alignTable(tbl...))
			stdoutln("[FlashNodes Idle]")
			tbl = showFlashNodesView(idleNodes, true, nil, table{formatFlashNodeViewTableTitle})
			stdoutln(alignTable(tbl...))
			return
		},
	}
	cmd.Flags().StringVarP(&name, "topoName", "n", proto.DefaultTopoName, "flash topology name")
	cmd.Flags().BoolVar(&showAllTopo, "showAllTopo", false, "list across all topologies (default false)")
	return cmd
}

func parseFlashGroupID(id string) (uint64, error) {
	return strconv.ParseUint(id, 10, 64)
}

const fullDot = ".................................................."

func proportion(s, e uint32) string {
	p := "."
	if n := int(float64(e-s) * float64(len(fullDot)) / float64(math.MaxUint32)); n > 0 {
		p = fullDot[:n]
	}
	return p
}
