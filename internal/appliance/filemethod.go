package appliance

import (
	"context"
	"fmt"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/linode"
)

// File-transfer migration method (see docs/FILE-MIGRATION.md).
//
// This method copies the source's FILES (only used storage) onto a freshly
// launched destination Linode running an OS image the operator picks — rather
// than a block-for-block image of the whole disk. It is a wholly ADDITIVE third
// method: every file-specific code path guards on isFileMethod, so the existing
// "volume" and "disk" block methods are never affected.

// isFileMethod reports whether a boot target is the file-transfer method. It is
// the single predicate every file-specific branch guards on.
func isFileMethod(bootTarget string) bool { return bootTarget == api.BootTargetFile }

// provisionsBlockStorage reports whether a boot target creates a Linode Block
// Storage replication volume at create time. Only the block methods do; the
// file method streams to a launched destination instead, so it provisions none.
func provisionsBlockStorage(bootTarget string) bool { return !isFileMethod(bootTarget) }

// fileHeadroom is the fraction of extra plan disk required beyond the source's
// used data, to leave room for the destination OS itself, logs and growth.
const fileHeadroom = 1.30

// validateFileCreate resolves and validates a file-transfer create request: it
// must have a single source entry (the whole filesystem, sized by USED bytes),
// a destination OS image, and a plan whose local disk fits used*headroom. It
// mutates req.LinodeType when the console left the plan to the appliance.
func (s *Server) validateFileCreate(ctx context.Context, req *api.CreateMigrationRequest, usedBytes int64) error {
	if len(req.Devices) != 1 {
		return fmt.Errorf("file migration copies the whole filesystem as one item — enter a single source with its USED storage, not per-disk entries")
	}
	if req.OSImage == "" {
		return fmt.Errorf("choose the destination OS image (match it to the source's OS)")
	}
	cl, ok := s.linodeClient(ctx)
	if !ok {
		// No token/automation (file-fallback tests): accept as-is; the launch
		// step is what actually needs the token and fails clearly there.
		return nil
	}
	if err := s.validateOSImage(ctx, cl, req.OSImage); err != nil {
		return err
	}
	types, err := cl.ListTypes(ctx)
	if err != nil {
		return fmt.Errorf("could not load Linode plans: %w", err)
	}
	needBytes := int64(float64(usedBytes) * fileHeadroom)
	if req.LinodeType != "" {
		var chosen *linode.LinodeType
		for i := range types {
			if types[i].ID == req.LinodeType {
				chosen = &types[i]
				break
			}
		}
		if chosen == nil || !linode.PlanClasses(req.PlanClass)[chosen.Class] {
			return fmt.Errorf("the selected plan is not a %s plan — pick one from the list", req.PlanClass)
		}
		if int64(chosen.DiskMB)*1024*1024 < needBytes {
			return fmt.Errorf("the selected plan's %d GB disk is too small for %s of used data plus room for the OS — pick a larger plan", chosen.DiskMB/1024, humanBytes(usedBytes))
		}
		return nil
	}
	plan, ok := linode.ClosestType(types, req.PlanClass, needBytes)
	if !ok {
		return fmt.Errorf("no %s Linode plan is large enough for %s of used data plus OS headroom — pick a larger plan class", req.PlanClass, humanBytes(usedBytes))
	}
	req.LinodeType = plan.ID
	return nil
}

// validateOSImage checks the chosen image id exists in the account's available
// Linode images (deployable public + private). A wrong id would only fail much
// later at launch, so reject it now.
func (s *Server) validateOSImage(ctx context.Context, cl *linode.Client, image string) error {
	images, err := cl.ListImages(ctx)
	if err != nil {
		// Don't hard-fail create on a transient images-list error; the launch
		// step validates for real.
		return nil
	}
	for _, im := range images {
		if im.ID == image {
			return nil
		}
	}
	return fmt.Errorf("destination OS image %q is not available on this account — pick one from the list", image)
}
