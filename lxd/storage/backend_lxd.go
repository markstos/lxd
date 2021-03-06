package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/storage/locking"
	"github.com/lxc/lxd/lxd/storage/memorypipe"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
)

type lxdBackend struct {
	driver drivers.Driver
	id     int64
	db     api.StoragePool
	name   string
	state  *state.State
	logger logger.Logger
}

// ID returns the storage pool ID.
func (b *lxdBackend) ID() int64 {
	return b.id
}

// Name returns the storage pool name.
func (b *lxdBackend) Name() string {
	return b.name
}

// Driver returns the storage pool driver.
func (b *lxdBackend) Driver() drivers.Driver {
	return b.driver
}

// MigrationTypes returns the migration transport method preferred when sending a migration,
// based on the migration method requested by the driver's ability.
func (b *lxdBackend) MigrationTypes(contentType drivers.ContentType, refresh bool) []migration.Type {
	return b.driver.MigrationTypes(contentType, refresh)
}

// create creates the storage pool layout on the storage device.
// localOnly is used for clustering where only a single node should do remote storage setup.
func (b *lxdBackend) create(localOnly bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"config": b.db.Config, "description": b.db.Description, "localOnly": localOnly})
	logger.Debug("create started")
	defer logger.Debug("created finished")

	revert := revert.New()
	defer revert.Fail()

	// Create the storage path.
	path := drivers.GetPoolMountPath(b.name)
	err := os.MkdirAll(path, 0711)
	if err != nil && !os.IsExist(err) {
		return errors.Wrapf(err, "Failed to create directory '%s'", path)
	}

	revert.Add(func() { os.RemoveAll(path) })

	// If dealing with a remote storage pool, we're done now.
	if b.driver.Info().Remote && localOnly {
		revert.Success()
		return nil
	}

	// Validate config.
	err = b.driver.Validate(b.db.Config)
	if err != nil {
		return err
	}

	// Create the storage pool on the storage device.
	err = b.driver.Create()
	if err != nil {
		return err
	}

	// Mount the storage pool.
	ourMount, err := b.driver.Mount()
	if err != nil {
		return err
	}

	// We expect the caller of create to mount the pool if needed, so we should unmount after
	// storage struct has been created.
	if ourMount {
		defer b.driver.Unmount()
	}

	// Create the directory structure.
	err = b.createStorageStructure(path)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// newVolume returns a new Volume instance containing copies of the supplied volume config and the pools config,
func (b *lxdBackend) newVolume(volType drivers.VolumeType, contentType drivers.ContentType, volName string, volConfig map[string]string) drivers.Volume {
	// Copy the config map to avoid internal modifications affecting external state.
	newConfig := map[string]string{}
	for k, v := range volConfig {
		newConfig[k] = v
	}

	// Copy the pool config map to avoid internal modifications affecting external state.
	newPoolConfig := map[string]string{}
	for k, v := range b.db.Config {
		newPoolConfig[k] = v
	}

	return drivers.NewVolume(b.driver, b.name, volType, contentType, volName, newConfig, newPoolConfig)
}

// GetResources returns utilisation information about the pool.
func (b *lxdBackend) GetResources() (*api.ResourcesStoragePool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("GetResources started")
	defer logger.Debug("GetResources finished")

	return b.driver.GetResources()
}

// Update updates the pool config.
func (b *lxdBackend) Update(driverOnly bool, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("Update started")
	defer logger.Debug("Update finished")

	// Validate config.
	err := b.driver.Validate(newConfig)
	if err != nil {
		return err
	}

	// Diff the configurations.
	changedConfig, userOnly := b.detectChangedConfig(b.db.Config, newConfig)

	// Apply config changes if there are any.
	if len(changedConfig) != 0 {
		if !userOnly {
			err = b.driver.Update(changedConfig)
			if err != nil {
				return err
			}
		}
	}

	// If only dealing with driver changes, we're done now.
	if driverOnly {
		return nil
	}

	// Update the database if something changed.
	if len(changedConfig) != 0 || newDesc != b.db.Description {
		err = b.state.Cluster.StoragePoolUpdate(b.name, newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	return nil

}

// Delete removes the pool.
func (b *lxdBackend) Delete(localOnly bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("Delete started")
	defer logger.Debug("Delete finished")

	// If completely gone, just return
	if !shared.PathExists(shared.VarPath("storage-pools", b.name)) {
		return nil
	}

	// Delete the low-level storage.
	if !localOnly || !b.driver.Info().Remote {
		err := b.driver.Delete(op)
		if err != nil {
			return err
		}
	} else {
		_, err := b.driver.Unmount()
		if err != nil {
			return err
		}
	}

	// Delete the mountpoint.
	path := shared.VarPath("storage-pools", b.name)
	err := os.Remove(path)
	if err != nil {
		return errors.Wrapf(err, "Failed to remove directory '%s'", path)
	}

	return nil
}

// Mount mounts the storage pool.
func (b *lxdBackend) Mount() (bool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("Mount started")
	defer logger.Debug("Mount finished")

	return b.driver.Mount()
}

// Unmount unmounts the storage pool.
func (b *lxdBackend) Unmount() (bool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("Unmount started")
	defer logger.Debug("Unmount finished")

	return b.driver.Unmount()
}

// ApplyPatch runs the requested patch at both backend and driver level.
func (b *lxdBackend) ApplyPatch(name string) error {
	// Run early backend patches.
	patch, ok := lxdEarlyPatches[name]
	if ok {
		err := patch(b)
		if err != nil {
			return err
		}
	}

	// Run the driver patch itself.
	err := b.driver.ApplyPatch(name)
	if err != nil {
		return err
	}

	// Run late backend patches.
	patch, ok = lxdLatePatches[name]
	if ok {
		err := patch(b)
		if err != nil {
			return err
		}
	}

	return nil
}

// ensureInstanceSymlink creates a symlink in the instance directory to the instance's mount path
// if doesn't exist already.
func (b *lxdBackend) ensureInstanceSymlink(instanceType instancetype.Type, projectName, instanceName, mountPath string) error {
	if shared.IsSnapshot(instanceName) {
		return fmt.Errorf("Instance must not be snapshot")
	}

	symlinkPath := InstancePath(instanceType, projectName, instanceName, false)

	// Remove any old symlinks left over by previous bugs that may point to a different pool.
	if shared.PathExists(symlinkPath) {
		err := os.Remove(symlinkPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink '%s'", symlinkPath)
		}
	}

	// Create new symlink.
	err := os.Symlink(mountPath, symlinkPath)
	if err != nil {
		return errors.Wrapf(err, "Failed to create symlink from '%s' to '%s'", mountPath, symlinkPath)
	}

	return nil
}

// removeInstanceSymlink removes a symlink in the instance directory to the instance's mount path.
func (b *lxdBackend) removeInstanceSymlink(instanceType instancetype.Type, projectName, instanceName string) error {
	symlinkPath := InstancePath(instanceType, projectName, instanceName, false)

	if shared.PathExists(symlinkPath) {
		err := os.Remove(symlinkPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink '%s'", symlinkPath)
		}
	}

	return nil
}

// ensureInstanceSnapshotSymlink creates a symlink in the snapshot directory to the instance's
// snapshot path if doesn't exist already.
func (b *lxdBackend) ensureInstanceSnapshotSymlink(instanceType instancetype.Type, projectName, instanceName string) error {
	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(instanceType)
	if err != nil {
		return err
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(instanceName)
	snapshotSymlink := InstancePath(instanceType, projectName, parentName, true)
	volStorageName := project.Prefix(projectName, parentName)

	snapshotTargetPath := drivers.GetVolumeSnapshotDir(b.name, volType, volStorageName)

	// Remove any old symlinks left over by previous bugs that may point to a different pool.
	if shared.PathExists(snapshotSymlink) {
		err = os.Remove(snapshotSymlink)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink '%s'", snapshotSymlink)
		}
	}

	// Create new symlink.
	err = os.Symlink(snapshotTargetPath, snapshotSymlink)
	if err != nil {
		return errors.Wrapf(err, "Failed to create symlink from '%s' to '%s'", snapshotTargetPath, snapshotSymlink)
	}

	return nil
}

// removeInstanceSnapshotSymlinkIfUnused removes the symlink in the snapshot directory to the
// instance's snapshot path if the snapshot path is missing. It is expected that the driver will
// remove the instance's snapshot path after the last snapshot is removed or the volume is deleted.
func (b *lxdBackend) removeInstanceSnapshotSymlinkIfUnused(instanceType instancetype.Type, projectName, instanceName string) error {
	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(instanceType)
	if err != nil {
		return err
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(instanceName)
	snapshotSymlink := InstancePath(instanceType, projectName, parentName, true)
	volStorageName := project.Prefix(projectName, parentName)

	snapshotTargetPath := drivers.GetVolumeSnapshotDir(b.name, volType, volStorageName)

	// If snapshot parent directory doesn't exist, remove symlink.
	if !shared.PathExists(snapshotTargetPath) {
		if shared.PathExists(snapshotSymlink) {
			err := os.Remove(snapshotSymlink)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove symlink '%s'", snapshotSymlink)
			}
		}
	}

	return nil
}

// instanceRootVolumeConfig returns the instance's root volume config.
func (b *lxdBackend) instanceRootVolumeConfig(inst instance.Instance) (map[string]string, error) {
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return nil, err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return nil, err
	}

	// Get volume config.
	_, vol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil, fmt.Errorf("Volume doesn't exist")
		}

		return nil, err
	}

	// Get the root disk device config.
	_, rootDiskConf, err := shared.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	// Override size property from instance root device config.
	if rootDiskConf["size"] != "" {
		vol.Config["size"] = rootDiskConf["size"]
	}

	return vol.Config, nil
}

// CreateInstance creates an empty instance.
func (b *lxdBackend) CreateInstance(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("CreateInstance started")
	defer logger.Debug("CreateInstance finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	revert := true
	defer func() {
		if !revert {
			return
		}
		b.DeleteInstance(inst, op)
	}()

	contentType := InstanceContentType(inst)

	// Find the root device config for instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.CreateVolume(vol, nil, op)
	if err != nil {
		return err
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply("create")
	if err != nil {
		return err
	}

	revert = false
	return nil
}

// CreateInstanceFromBackup restores a backup file onto the storage device. Because the backup file
// is unpacked and restored onto the storage device before the instance is created in the database
// it is necessary to return two functions; a post hook that can be run once the instance has been
// created in the database to run any storage layer finalisations, and a revert hook that can be
// run if the instance database load process fails that will remove anything created thus far.
func (b *lxdBackend) CreateInstanceFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (func(instance.Instance) error, func(), error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": srcBackup.Project, "instance": srcBackup.Name, "snapshots": srcBackup.Snapshots, "optimizedStorage": srcBackup.OptimizedStorage})
	logger.Debug("CreateInstanceFromBackup started")
	defer logger.Debug("CreateInstanceFromBackup finished")

	// Get the volume name on storage.
	volStorageName := project.Prefix(srcBackup.Project, srcBackup.Name)

	// Currently there is no concept of instance type in backups, so we assume container.
	// We don't know the volume's config yet as tarball hasn't been unpacked.
	// We will apply the config as part of the post hook function returned if driver needs to.
	vol := b.newVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, volStorageName, nil)

	revert := revert.New()
	defer revert.Fail()

	// Unpack the backup into the new storage volume(s).
	volPostHook, revertHook, err := b.driver.CreateVolumeFromBackup(vol, srcBackup.Snapshots, srcData, srcBackup.OptimizedStorage, op)
	if err != nil {
		return nil, nil, err
	}

	if revertHook != nil {
		revert.Add(revertHook)
	}

	err = b.ensureInstanceSymlink(instancetype.Container, srcBackup.Project, srcBackup.Name, vol.MountPath())
	if err != nil {
		return nil, nil, err
	}

	revert.Add(func() {
		b.removeInstanceSymlink(instancetype.Container, srcBackup.Project, srcBackup.Name)
	})

	if len(srcBackup.Snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(instancetype.Container, srcBackup.Project, srcBackup.Name)
		if err != nil {
			return nil, nil, err
		}

		revert.Add(func() {
			b.removeInstanceSnapshotSymlinkIfUnused(instancetype.Container, srcBackup.Project, srcBackup.Name)
		})
	}

	// Update pool information in the backup.yaml file.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		return backup.UpdateInstanceConfigStoragePool(b.state.Cluster, srcBackup, mountPath)
	}, op)
	if err != nil {
		return nil, nil, err
	}

	var postHook func(instance.Instance) error

	// Create a post hook function that will use the instance (that will be created) to setup a new volume
	// containing the instance's root disk device's config so that the driver's post hook function can access
	// that config to perform any post instance creation setup.
	postHook = func(inst instance.Instance) error {
		// Get the root disk device config.
		rootDiskConf, err := b.instanceRootVolumeConfig(inst)
		if err != nil {
			return err
		}

		// Get the volume name on storage.
		volStorageName := project.Prefix(inst.Project(), inst.Name())

		volType, err := InstanceTypeToVolumeType(inst.Type())
		if err != nil {
			return err
		}

		contentType := InstanceContentType(inst)

		// If the driver returned a post hook, run it now.
		if volPostHook != nil {
			// Initialise new volume containing root disk config supplied in instance.
			vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
			err = volPostHook(vol)
			if err != nil {
				return err
			}
		}

		// Apply quota config from root device if its set. Should be done after driver's post hook if set
		// so that any volume initialisation has been completed first.
		if rootDiskConf["size"] != "" {
			logger.Debug("Applying volume quota from root disk config", log.Ctx{"size": rootDiskConf["size"]})
			err = b.driver.SetVolumeQuota(vol, rootDiskConf["size"], op)
			if err != nil {
				return err
			}
		}

		return nil
	}

	revert.Success()
	return postHook, revertHook, nil
}

// CreateInstanceFromCopy copies an instance volume and optionally its snapshots to new volume(s).
func (b *lxdBackend) CreateInstanceFromCopy(inst instance.Instance, src instance.Instance, snapshots bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name(), "snapshots": snapshots})
	logger.Debug("CreateInstanceFromCopy started")
	defer logger.Debug("CreateInstanceFromCopy finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Initialise a new volume containing the root disk config supplied in the new instance.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	if b.driver.HasVolume(vol) {
		return fmt.Errorf("Cannot create volume, already exists on target")
	}

	// Get the src volume name on storage.
	srcVolStorageName := project.Prefix(src.Project(), src.Name())

	// We don't need to use the source instance's root disk config, so set to nil.
	srcVol := b.newVolume(volType, contentType, srcVolStorageName, nil)

	revert := true
	defer func() {
		if !revert {
			return
		}
		b.DeleteInstance(inst, op)
	}()

	srcPool, err := GetPoolByInstance(b.state, src)
	if err != nil {
		return err
	}

	if b.Name() == srcPool.Name() {
		logger.Debug("CreateInstanceFromCopy same-pool mode detected")
		err = b.driver.CreateVolumeFromCopy(vol, srcVol, snapshots, op)
		if err != nil {
			return err
		}
	} else {
		// We are copying volumes between storage pools so use migration system as it will
		// be able to negotiate a common transfer method between pool types.
		logger.Debug("CreateInstanceFromCopy cross-pool mode detected")

		// If we are copying snapshots, retrieve a list of snapshots from source volume.
		snapshotNames := []string{}
		if snapshots {
			snapshots, err := VolumeSnapshotsGet(b.state, srcPool.Name(), src.Name(), volDBType)
			if err != nil {
				return err
			}

			for _, snapshot := range snapshots {
				_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(snapshot.Name)
				snapshotNames = append(snapshotNames, snapShotName)
			}
		}

		// Use in-memory pipe pair to simulate a connection between the sender and receiver.
		aEnd, bEnd := memorypipe.NewPipePair()

		// Negotiate the migration type to use.
		offeredTypes := srcPool.MigrationTypes(contentType, false)
		offerHeader := migration.TypesToHeader(offeredTypes...)
		migrationType, err := migration.MatchTypes(offerHeader, migration.MigrationFSType_RSYNC, b.MigrationTypes(contentType, false))
		if err != nil {
			return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
		}

		// Run sender and receiver in separate go routines to prevent deadlocks.
		aEndErrCh := make(chan error, 1)
		bEndErrCh := make(chan error, 1)
		go func() {
			err := srcPool.MigrateInstance(src, aEnd, &migration.VolumeSourceArgs{
				Name:          src.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationType,
				TrackProgress: true, // Do use a progress tracker on sender.
			}, op)

			aEndErrCh <- err
		}()

		go func() {
			err := b.CreateInstanceFromMigration(inst, bEnd, migration.VolumeTargetArgs{
				Name:          inst.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationType,
				TrackProgress: false, // Do not use a progress tracker on receiver.
			}, op)

			bEndErrCh <- err
		}()

		// Capture errors from the sender and receiver from their result channels.
		errs := []error{}
		aEndErr := <-aEndErrCh
		if aEndErr != nil {
			errs = append(errs, aEndErr)
		}

		bEndErr := <-bEndErrCh
		if bEndErr != nil {
			errs = append(errs, bEndErr)
		}

		if len(errs) > 0 {
			return fmt.Errorf("Create instance volume from copy failed: %v", errs)
		}
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply("copy")
	if err != nil {
		return err
	}

	revert = false
	return nil
}

// RefreshInstance synchronises one instance's volume (and optionally snapshots) over another.
// Snapshots that are not present in the source but are in the destination are removed from the
// destination if snapshots are included in the synchronisation.
func (b *lxdBackend) RefreshInstance(inst instance.Instance, src instance.Instance, srcSnapshots []instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name(), "srcSnapshots": len(srcSnapshots)})
	logger.Debug("RefreshInstance started")
	defer logger.Debug("RefreshInstance finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Initialise a new volume containing the root disk config supplied in the new instance.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	// Get the src volume name on storage.
	srcVolStorageName := project.Prefix(src.Project(), src.Name())

	// We don't need to use the source instance's root disk config, so set to nil.
	srcVol := b.newVolume(volType, contentType, srcVolStorageName, nil)

	srcSnapVols := []drivers.Volume{}
	for _, snapInst := range srcSnapshots {
		// Initialise a new volume containing the root disk config supplied in the
		// new instance. We don't need to use the source instance's snapshot root
		// disk config, so set to nil. This is because snapshots are immutable yet
		// the instance and its snapshots can be transferred between pools, so using
		// the data from the snapshot is incorrect.

		// Get the snap volume name on storage.
		snapVolStorageName := project.Prefix(snapInst.Project(), snapInst.Name())
		srcSnapVol := b.newVolume(volType, contentType, snapVolStorageName, nil)
		srcSnapVols = append(srcSnapVols, srcSnapVol)
	}

	srcPool, err := GetPoolByInstance(b.state, src)
	if err != nil {
		return err
	}

	if b.Name() == srcPool.Name() {
		logger.Debug("RefreshInstance same-pool mode detected")
		err = b.driver.RefreshVolume(vol, srcVol, srcSnapVols, op)
		if err != nil {
			return err
		}
	} else {
		// We are copying volumes between storage pools so use migration system as it will
		// be able to negotiate a common transfer method between pool types.
		logger.Debug("RefreshInstance cross-pool mode detected")

		// Retrieve a list of snapshots we are copying.
		snapshotNames := []string{}
		for _, srcSnapVol := range srcSnapVols {
			_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapVol.Name())
			snapshotNames = append(snapshotNames, snapShotName)
		}

		// Use in-memory pipe pair to simulate a connection between the sender and receiver.
		aEnd, bEnd := memorypipe.NewPipePair()

		// Negotiate the migration type to use.
		offeredTypes := srcPool.MigrationTypes(contentType, true)
		offerHeader := migration.TypesToHeader(offeredTypes...)
		migrationType, err := migration.MatchTypes(offerHeader, migration.MigrationFSType_RSYNC, b.MigrationTypes(contentType, true))
		if err != nil {
			return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
		}

		// Run sender and receiver in separate go routines to prevent deadlocks.
		aEndErrCh := make(chan error, 1)
		bEndErrCh := make(chan error, 1)
		go func() {
			err := srcPool.MigrateInstance(src, aEnd, &migration.VolumeSourceArgs{
				Name:          src.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationType,
				TrackProgress: true, // Do use a progress tracker on sender.
			}, op)

			aEndErrCh <- err
		}()

		go func() {
			err := b.CreateInstanceFromMigration(inst, bEnd, migration.VolumeTargetArgs{
				Name:          inst.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationType,
				Refresh:       true,  // Indicate to receiver volume should exist.
				TrackProgress: false, // Do not use a progress tracker on receiver.
			}, op)

			bEndErrCh <- err
		}()

		// Capture errors from the sender and receiver from their result channels.
		errs := []error{}
		aEndErr := <-aEndErrCh
		if aEndErr != nil {
			errs = append(errs, aEndErr)
		}

		bEndErr := <-bEndErrCh
		if bEndErr != nil {
			errs = append(errs, bEndErr)
		}

		if len(errs) > 0 {
			return fmt.Errorf("Create instance volume from copy failed: %v", errs)
		}
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply("copy")
	if err != nil {
		return err
	}

	return nil
}

// imageFiller returns a function that can be used as a filler function with CreateVolume().
// The function returned will unpack the specified image archive into the specified mount path
// provided, and for VM images, a raw root block path is required to unpack the qcow2 image into.
func (b *lxdBackend) imageFiller(fingerprint string, op *operations.Operation) func(mountPath, rootBlockPath string) error {
	return func(mountPath, rootBlockPath string) error {
		var tracker *ioprogress.ProgressTracker
		if op != nil { // Not passed when being done as part of pre-migration setup.
			metadata := make(map[string]interface{})
			tracker = &ioprogress.ProgressTracker{
				Handler: func(percent, speed int64) {
					shared.SetProgressMetadata(metadata, "create_instance_from_image_unpack", "Unpack", percent, 0, speed)
					op.UpdateMetadata(metadata)
				}}
		}
		imageFile := shared.VarPath("images", fingerprint)
		return ImageUnpack(imageFile, mountPath, rootBlockPath, b.driver.Info().BlockBacking, b.state.OS.RunningInUserNS, tracker)
	}
}

// CreateInstanceFromImage creates a new volume for an instance populated with the image requested.
func (b *lxdBackend) CreateInstanceFromImage(inst instance.Instance, fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("CreateInstanceFromImage started")
	defer logger.Debug("CreateInstanceFromImage finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	revert := true
	defer func() {
		if !revert {
			return
		}
		b.DeleteInstance(inst, op)
	}()

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	// If the driver doesn't support optimized image volumes then create a new empty volume and
	// populate it with the contents of the image archive.
	if !b.driver.Info().OptimizedImages {
		volFiller := drivers.VolumeFiller{
			Fingerprint: fingerprint,
			Fill:        b.imageFiller(fingerprint, op),
		}

		err = b.driver.CreateVolume(vol, &volFiller, op)
		if err != nil {
			return err
		}
	} else {
		// If the driver does support optimized images then ensure the optimized image
		// volume has been created for the archive's fingerprint and then proceed to create
		// a new volume by copying the optimized image volume.
		err := b.EnsureImage(fingerprint, op)
		if err != nil {
			return err
		}

		// No config for an image volume so set to nil.
		imgVol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)
		err = b.driver.CreateVolumeFromCopy(vol, imgVol, false, op)
		if err != nil {
			return err
		}
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply("create")
	if err != nil {
		return err
	}

	revert = false
	return nil
}

// CreateInstanceFromMigration receives an instance being migrated.
// The args.Name and args.Config fields are ignored and, instance properties are used instead.
func (b *lxdBackend) CreateInstanceFromMigration(inst instance.Instance, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "args": args})
	logger.Debug("CreateInstanceFromMigration started")
	defer logger.Debug("CreateInstanceFromMigration finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Find the root device config for instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Override args.Name and args.Config to ensure volume is created based on instance.
	args.Config = rootDiskConf
	args.Name = inst.Name()

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), args.Name)

	vol := b.newVolume(volType, contentType, volStorageName, args.Config)

	volExists := b.driver.HasVolume(vol)
	if args.Refresh && !volExists {
		return fmt.Errorf("Cannot refresh volume, doesn't exist on target")
	} else if !args.Refresh && volExists {
		return fmt.Errorf("Cannot create volume, already exists on target")
	}

	var preFiller drivers.VolumeFiller

	revert := true

	if !args.Refresh {
		defer func() {
			if !revert {
				return
			}
			b.DeleteInstance(inst, op)
		}()

		// If the negotiated migration method is rsync and the instance's base image is
		// already on the host then setup a pre-filler that will unpack the local image
		// to try and speed up the rsync of the incoming volume by avoiding the need to
		// transfer the base image files too.
		if args.MigrationType.FSType == migration.MigrationFSType_RSYNC {
			fingerprint := inst.ExpandedConfig()["volatile.base_image"]
			_, _, err = b.state.Cluster.ImageGet(inst.Project(), fingerprint, false, true)
			if err != db.ErrNoSuchObject && err != nil {
				return err
			}

			if err == nil {
				logger.Debug("Using optimised migration from existing image", log.Ctx{"fingerprint": fingerprint})

				// Populate the volume filler with the fingerprint and image filler
				// function that can be used by the driver to pre-populate the
				// volume with the contents of the image.
				preFiller = drivers.VolumeFiller{
					Fingerprint: fingerprint,
					Fill:        b.imageFiller(fingerprint, op),
				}

				// Ensure if the image doesn't yet exist on a driver which supports
				// optimized storage, then it gets created first.
				err = b.EnsureImage(preFiller.Fingerprint, op)
				if err != nil {
					return err
				}
			}
		}
	}

	err = b.driver.CreateVolumeFromMigration(vol, conn, args, &preFiller, op)
	if err != nil {
		conn.Close()
		return err
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	if len(args.Snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
		if err != nil {
			return err
		}
	}

	revert = false
	return nil
}

// RenameInstance renames the instance's root volume and any snapshot volumes.
func (b *lxdBackend) RenameInstance(inst instance.Instance, newName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newName": newName})
	logger.Debug("RenameInstance started")
	defer logger.Debug("RenameInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance cannot be a snapshot")
	}

	if shared.IsSnapshot(newName) {
		return fmt.Errorf("New name cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// Get any snapshots the instance has in the format <instance name>/<snapshot name>.
	snapshots, err := b.state.Cluster.ContainerGetSnapshots(inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		revert.Add(func() {
			b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), newName)
			b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
		})
	}

	// Rename each snapshot DB record to have the new parent volume prefix.
	for _, srcSnapshot := range snapshots {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot)
		newSnapVolName := drivers.GetSnapshotVolumeName(newName, snapName)
		err = b.state.Cluster.StoragePoolVolumeRename(inst.Project(), srcSnapshot, newSnapVolName, volDBType, b.ID())
		if err != nil {
			return err
		}

		revert.Add(func() {
			b.state.Cluster.StoragePoolVolumeRename(inst.Project(), newSnapVolName, srcSnapshot, volDBType, b.ID())
		})
	}

	// Rename the parent volume DB record.
	err = b.state.Cluster.StoragePoolVolumeRename(inst.Project(), inst.Name(), newName, volDBType, b.ID())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.state.Cluster.StoragePoolVolumeRename(inst.Project(), newName, inst.Name(), volDBType, b.ID())
	})

	// Rename the volume and its snapshots on the storage device.
	volStorageName := project.Prefix(inst.Project(), inst.Name())
	newVolStorageName := project.Prefix(inst.Project(), newName)
	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when renaming a volume.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	err = b.driver.RenameVolume(vol, newVolStorageName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		// There's no need to pass config as it's not needed when renaming a volume.
		newVol := b.newVolume(volType, contentType, newVolStorageName, nil)
		b.driver.RenameVolume(newVol, volStorageName, op)
	})

	// Remove old instance symlink and create new one.
	err = b.removeInstanceSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), drivers.GetVolumeMountPath(b.name, volType, volStorageName))
	})

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), newName, drivers.GetVolumeMountPath(b.name, volType, newVolStorageName))
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.removeInstanceSymlink(inst.Type(), inst.Project(), newName)
	})

	// Remove old instance snapshot symlink and create a new one if needed.
	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), newName)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// DeleteInstance removes the instance's root volume (all snapshots need to be removed first).
func (b *lxdBackend) DeleteInstance(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("DeleteInstance started")
	defer logger.Debug("DeleteInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance must not be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	// Get any snapshots the instance has in the format <instance name>/<snapshot name>.
	snapshots, err := b.state.Cluster.ContainerGetSnapshots(inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Check all snapshots are already removed.
	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove an instance volume that has snapshots")
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())
	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when deleting a volume.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Delete the volume from the storage device. Must come after snapshots are removed.
	// Must come before DB StoragePoolVolumeDelete so that the volume ID is still available.
	logger.Debug("Deleting instance volume", log.Ctx{"volName": volStorageName})
	err = b.driver.DeleteVolume(vol, op)
	if err != nil {
		return err
	}

	// Remove symlinks.
	err = b.removeInstanceSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Remove the volume record from the database.
	err = b.state.Cluster.StoragePoolVolumeDelete(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		return err
	}

	return nil
}

// UpdateInstance updates an instance volume's config.
func (b *lxdBackend) UpdateInstance(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateInstance started")
	defer logger.Debug("UpdateInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	volStorageName := project.Prefix(inst.Project(), inst.Name())
	contentType := InstanceContentType(inst)

	// Validate config.
	newVol := b.newVolume(volType, contentType, volStorageName, newConfig)
	err = b.driver.ValidateVolume(newVol, false)
	if err != nil {
		return err
	}

	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Volume doesn't exist")
		}

		return err
	}

	// Apply config changes if there are any.
	changedConfig, userOnly := b.detectChangedConfig(curVol.Config, newConfig)
	if len(changedConfig) != 0 {
		// Check that the volume's size property isn't being changed.
		if changedConfig["size"] != "" {
			return fmt.Errorf("Instance volume 'size' property cannot be changed")
		}

		// Check that the volume's block.filesystem property isn't being changed.
		if changedConfig["block.filesystem"] != "" {
			return fmt.Errorf("Instance volume 'block.filesystem' property cannot be changed")
		}

		// Get the root disk device config.
		rootDiskConf, err := b.instanceRootVolumeConfig(inst)
		if err != nil {
			return err
		}

		curVol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
		if !userOnly {
			err = b.driver.UpdateVolume(curVol, changedConfig)
			if err != nil {
				return err
			}
		}
	}

	// Update the database if something changed.
	if len(changedConfig) != 0 || newDesc != curVol.Description {
		err = b.state.Cluster.StoragePoolVolumeUpdateByProject(inst.Project(), inst.Name(), volDBType, b.ID(), newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateInstanceSnapshot updates an instance snapshot volume's description.
// Volume config is not allowed to be updated and will return an error.
func (b *lxdBackend) UpdateInstanceSnapshot(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateInstanceSnapshot started")
	defer logger.Debug("UpdateInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	return b.updateVolumeDescriptionOnly(inst.Project(), inst.Name(), volDBType, newDesc, newConfig)
}

// MigrateInstance sends an instance volume for migration.
// The args.Name field is ignored and the name of the instance is used instead.
func (b *lxdBackend) MigrateInstance(inst instance.Instance, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "args": args})
	logger.Debug("MigrateInstance started")
	defer logger.Debug("MigrateInstance finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	if len(args.Snapshots) > 0 && args.FinalSync {
		return fmt.Errorf("Snapshots should not be transferred during final sync")
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	args.Name = inst.Name() // Override args.Name to ensure instance volume is sent.

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), args.Name)

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.MigrateVolume(vol, conn, args, op)
	if err != nil {
		return err
	}

	return nil
}

// BackupInstance creates an instance backup.
func (b *lxdBackend) BackupInstance(inst instance.Instance, targetPath string, optimized bool, snapshots bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "targetPath": targetPath, "optimized": optimized, "snapshots": snapshots})
	logger.Debug("BackupInstance started")
	defer logger.Debug("BackupInstance finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Ensure the backup file reflects current config.
	err = b.UpdateInstanceBackupFile(inst, op)
	if err != nil {
		return err
	}

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.BackupVolume(vol, targetPath, optimized, snapshots, op)
	if err != nil {
		return err
	}

	return nil
}

// GetInstanceUsage returns the disk usage of the instance's root volume.
func (b *lxdBackend) GetInstanceUsage(inst instance.Instance) (int64, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("GetInstanceUsage started")
	defer logger.Debug("GetInstanceUsage finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return -1, err
	}

	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when retrieving the volume usage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	return b.driver.GetVolumeUsage(vol)
}

// SetInstanceQuota sets the quota on the instance's root volume.
// Returns ErrRunningQuotaResizeNotSupported if the instance is running and the storage driver
// doesn't support resizing whilst the instance is running.
func (b *lxdBackend) SetInstanceQuota(inst instance.Instance, size string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("SetInstanceQuota started")
	defer logger.Debug("SetInstanceQuota finished")

	if inst.IsRunning() && !b.driver.Info().RunningQuotaResize {
		return ErrRunningQuotaResizeNotSupported
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentVolume := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	// There's no need to pass config as it's not needed when setting quotas.
	vol := b.newVolume(volType, contentVolume, volStorageName, nil)

	return b.driver.SetVolumeQuota(vol, size, op)
}

// MountInstance mounts the instance's root volume.
func (b *lxdBackend) MountInstance(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("MountInstance started")
	defer logger.Debug("MountInstance finished")

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return false, err
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.MountVolume(vol, op)
}

// UnmountInstance unmounts the instance's root volume.
func (b *lxdBackend) UnmountInstance(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UnmountInstance started")
	defer logger.Debug("UnmountInstance finished")

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return false, err
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.UnmountVolume(vol, op)
}

// GetInstanceDisk returns the location of the disk.
func (b *lxdBackend) GetInstanceDisk(inst instance.Instance) (string, error) {
	if inst.Type() != instancetype.VM {
		return "", ErrNotImplemented
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return "", err
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	// There's no need to pass config as it's not needed when getting the
	// location of the disk block device.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Get the location of the disk block device.
	diskPath, err := b.driver.GetVolumeDiskPath(vol)
	if err != nil {
		return "", err
	}

	return diskPath, nil
}

// CreateInstanceSnapshot creates a snaphot of an instance volume.
func (b *lxdBackend) CreateInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name()})
	logger.Debug("CreateInstanceSnapshot started")
	defer logger.Debug("CreateInstanceSnapshot finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	if src.IsSnapshot() {
		return fmt.Errorf("Source instance cannot be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	// Some driver backing stores require that running instances be frozen during snapshot.
	if b.driver.Info().RunningSnapshotFreeze && src.IsRunning() {
		err = src.Freeze()
		if err != nil {
			return err
		}
		defer src.Unfreeze()
	}

	isSnap := inst.IsSnapshot()

	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	// There's no need to pass config as it's not needed when creating volume
	// snapshots.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	err = b.driver.CreateVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	return nil
}

// RenameInstanceSnapshot renames an instance snapshot.
func (b *lxdBackend) RenameInstanceSnapshot(inst instance.Instance, newName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newName": newName})
	logger.Debug("RenameInstanceSnapshot started")
	defer logger.Debug("RenameInstanceSnapshot finished")

	revert := revert.New()
	defer revert.Fail()

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	if shared.IsSnapshot(newName) {
		return fmt.Errorf("New name cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	parentName, oldSnapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(inst.Name())
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Rename storage volume snapshot. No need to pass config as it's not needed when renaming a volume.
	snapVol := b.newVolume(volType, contentType, volStorageName, nil)
	err = b.driver.RenameVolumeSnapshot(snapVol, newName, op)
	if err != nil {
		return err
	}

	newVolName := drivers.GetSnapshotVolumeName(parentName, newName)

	revert.Add(func() {
		// Revert rename. No need to pass config as it's not needed when renaming a volume.
		newSnapVol := b.newVolume(volType, contentType, project.Prefix(inst.Project(), newVolName), nil)
		b.driver.RenameVolumeSnapshot(newSnapVol, oldSnapshotName, op)
	})

	// Rename DB volume record.
	err = b.state.Cluster.StoragePoolVolumeRename(inst.Project(), inst.Name(), newVolName, volDBType, b.ID())
	if err != nil {
		return err
	}

	revert.Add(func() {
		// Rename DB volume record back.
		b.state.Cluster.StoragePoolVolumeRename(inst.Project(), newVolName, inst.Name(), volDBType, b.ID())
	})

	// Ensure the backup file reflects current config.
	err = b.UpdateInstanceBackupFile(inst, op)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// DeleteInstanceSnapshot removes the snapshot volume for the supplied snapshot instance.
func (b *lxdBackend) DeleteInstanceSnapshot(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("DeleteInstanceSnapshot started")
	defer logger.Debug("DeleteInstanceSnapshot finished")

	parentName, snapName, isSnap := shared.InstanceGetParentAndSnapshotName(inst.Name())
	if !inst.IsSnapshot() || !isSnap {
		return fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the parent volume name on storage.
	parentStorageName := project.Prefix(inst.Project(), parentName)

	// Delete the snapshot from the storage device.
	// Must come before DB StoragePoolVolumeDelete so that the volume ID is still available.
	logger.Debug("Deleting instance snapshot volume", log.Ctx{"volName": parentStorageName, "snapshotName": snapName})

	snapVolName := drivers.GetSnapshotVolumeName(parentStorageName, snapName)

	// There's no need to pass config as it's not needed when deleting a volume
	// snapshot.
	vol := b.newVolume(volType, contentType, snapVolName, nil)

	err = b.driver.DeleteVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	// Delete symlink if needed.
	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Remove the snapshot volume record from the database.
	err = b.state.Cluster.StoragePoolVolumeDelete(inst.Project(), drivers.GetSnapshotVolumeName(parentName, snapName), volDBType, b.ID())
	if err != nil {
		return err
	}

	return nil
}

// RestoreInstanceSnapshot restores an instance snapshot.
func (b *lxdBackend) RestoreInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name()})
	logger.Debug("RestoreInstanceSnapshot started")
	defer logger.Debug("RestoreInstanceSnapshot finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance must not be snapshot")
	}

	if !src.IsSnapshot() {
		return fmt.Errorf("Source instance must be a snapshot")
	}

	// Target instance must not be running.
	if inst.IsRunning() {
		return fmt.Errorf("Instance must not be running to restore")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Find the root device config for source snapshot instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	_, snapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(src.Name())
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	// Use the source snapshot's rootfs config (as this will later be restored into inst too).
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.RestoreVolume(vol, snapshotName, op)
	if err != nil {
		snapErr, ok := err.(drivers.ErrDeleteSnapshots)
		if ok {
			// We need to delete some snapshots and try again.
			snaps, err := inst.Snapshots()
			if err != nil {
				return err
			}

			// Go through all the snapshots.
			for _, snap := range snaps {
				_, snapName, _ := shared.InstanceGetParentAndSnapshotName(src.Name())
				if !shared.StringInSlice(snapName, snapErr.Snapshots) {
					continue
				}

				// Delete if listed.
				err := b.DeleteInstanceSnapshot(snap, op)
				if err != nil {
					return err
				}
			}

			// Now try again.
			err = b.driver.RestoreVolume(vol, snapshotName, op)
			if err != nil {
				return err
			}
		}

		return err
	}

	return nil
}

// MountInstanceSnapshot mounts an instance snapshot. It is mounted as read only so that the
// snapshot cannot be modified.
func (b *lxdBackend) MountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("MountInstanceSnapshot started")
	defer logger.Debug("MountInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return false, fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return false, err
	}

	// Get the parent and snapshot name.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.MountVolumeSnapshot(vol, op)
}

// UnmountInstanceSnapshot unmounts an instance snapshot.
func (b *lxdBackend) UnmountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UnmountInstanceSnapshot started")
	defer logger.Debug("UnmountInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return false, fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return false, err
	}

	contentType := InstanceContentType(inst)

	// Get the parent and snapshot name.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.UnmountVolumeSnapshot(vol, op)
}

// poolBlockFilesystem returns the filesystem used for new block device filesystems.
func (b *lxdBackend) poolBlockFilesystem() string {
	if b.db.Config["volume.block.filesystem"] != "" {
		return b.db.Config["volume.block.filesystem"]
	}

	return drivers.DefaultFilesystem
}

// EnsureImage creates an optimized volume of the image if supported by the storage pool driver and
// the volume doesn't already exist.
func (b *lxdBackend) EnsureImage(fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint})
	logger.Debug("EnsureImage started")
	defer logger.Debug("EnsureImage finished")

	if !b.driver.Info().OptimizedImages {
		return nil // Nothing to do for drivers that don't support optimized images volumes.
	}

	// We need to lock this operation to ensure that the image is not being created multiple times.
	// Uses a lock name of "EnsureImage_<fingerprint>" to avoid deadlocking with CreateVolume below that also
	// establishes a lock on the volume type & name if it needs to mount the volume before filling.
	unlock := locking.Lock(b.name, string(drivers.VolumeTypeImage), fmt.Sprintf("EnsureImage_%v", fingerprint))
	defer unlock()

	// Load image info from database.
	_, image, err := b.state.Cluster.ImageGetFromAnyProject(fingerprint)
	if err != nil {
		return err
	}

	// Derive content type from image type. Image types are not the same as instance types, so don't use
	// instance type constants for comparison.
	contentType := drivers.ContentTypeFS
	if image.Type == "virtual-machine" {
		contentType = drivers.ContentTypeBlock
	}

	// Try and load any existing volume config on this storage pool so we can compare filesystems if needed.
	_, imgDBVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
	if err != nil {
		if err != db.ErrNoSuchObject {
			return err
		}
	}

	// If an existing DB row was found, check if filesystem is the same as the current pool's filesystem.
	// If not we need to delete the existing cached image volume and re-create using new filesystem.
	if imgDBVol != nil && contentType == drivers.ContentTypeFS {
		if b.Driver().Info().BlockBacking && imgDBVol.Config["block.filesystem"] != b.poolBlockFilesystem() {
			logger.Debug("Filesystem of pool has changed since cached image volume created, regenerating image volume")
			err = b.DeleteImage(fingerprint, op)
			if err != nil {
				return err
			}
		}
	}

	// Create the new image volume. No config for an image volume so set to nil.
	// Pool config values will be read by the underlying driver if needed.
	imgVol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)

	// Check if we already have a suitable volume on storage device.
	if b.driver.HasVolume(imgVol) {
		return nil
	}

	volFiller := drivers.VolumeFiller{
		Fingerprint: fingerprint,
		Fill:        b.imageFiller(fingerprint, op),
	}

	err = b.driver.CreateVolume(imgVol, &volFiller, op)
	if err != nil {
		return err
	}

	err = VolumeDBCreate(b.state, "default", b.name, fingerprint, "", db.StoragePoolVolumeTypeNameImage, false, nil)
	if err != nil {
		return err
	}

	return nil
}

// DeleteImage removes an image from the database and underlying storage device if needed.
func (b *lxdBackend) DeleteImage(fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint})
	logger.Debug("DeleteImage started")
	defer logger.Debug("DeleteImage finished")

	// We need to lock this operation to ensure that the image is not being
	// deleted multiple times.
	unlock := locking.Lock(b.name, string(drivers.VolumeTypeImage), fingerprint)
	defer unlock()

	// Load image info from database.
	_, image, err := b.state.Cluster.ImageGetFromAnyProject(fingerprint)
	if err != nil {
		return err
	}

	contentType := drivers.ContentTypeFS

	// Image types are not the same as instance types, so don't use instance type constants.
	if image.Type == "virtual-machine" {
		contentType = drivers.ContentTypeBlock
	}

	vol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)

	err = b.driver.DeleteVolume(vol, op)
	if err != nil {
		return err
	}

	err = b.state.Cluster.StoragePoolVolumeDelete("default", fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
	if err != nil {
		return err
	}

	return nil
}

// updateVolumeDescriptionOnly is a helper function used when handling update requests for volumes
// that only allow their descriptions to be updated. If any config supplied differs from the
// current volume's config then an error is returned.
func (b *lxdBackend) updateVolumeDescriptionOnly(project, volName string, dbVolType int, newDesc string, newConfig map[string]string) error {
	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(project, volName, dbVolType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Volume doesn't exist")
		}

		return err
	}

	if newConfig != nil {
		changedConfig, _ := b.detectChangedConfig(curVol.Config, newConfig)
		if len(changedConfig) != 0 {
			return fmt.Errorf("Volume config is not editable")
		}
	}

	// Update the database if description changed. Use current config.
	if newDesc != curVol.Description {
		err = b.state.Cluster.StoragePoolVolumeUpdateByProject(project, volName, dbVolType, b.ID(), newDesc, curVol.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateImage updates image config.
func (b *lxdBackend) UpdateImage(fingerprint, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint, "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateImage started")
	defer logger.Debug("UpdateImage finished")

	return b.updateVolumeDescriptionOnly("default", fingerprint, db.StoragePoolVolumeTypeImage, newDesc, newConfig)
}

// CreateCustomVolume creates an empty custom volume.
func (b *lxdBackend) CreateCustomVolume(volName, desc string, config map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "desc": desc, "config": config})
	logger.Debug("CreateCustomVolume started")
	defer logger.Debug("CreateCustomVolume finished")

	// Validate config.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, config)
	err := b.driver.ValidateVolume(vol, false)
	if err != nil {
		return err
	}

	// Create database entry for new storage volume.
	err = VolumeDBCreate(b.state, "default", b.name, volName, desc, db.StoragePoolVolumeTypeNameCustom, false, vol.Config())
	if err != nil {
		return err
	}

	revertDB := true
	defer func() {
		if revertDB {
			b.state.Cluster.StoragePoolVolumeDelete("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	// Create the empty custom volume on the storage device.
	err = b.driver.CreateVolume(vol, nil, op)
	if err != nil {
		return err
	}

	revertDB = false
	return nil
}

// CreateCustomVolumeFromCopy creates a custom volume from an existing custom volume.
// It copies the snapshots from the source volume by default, but can be disabled if requested.
func (b *lxdBackend) CreateCustomVolumeFromCopy(volName, desc string, config map[string]string, srcPoolName, srcVolName string, srcVolOnly bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "desc": desc, "config": config, "srcPoolName": srcPoolName, "srcVolName": srcVolName, "srcVolOnly": srcVolOnly})
	logger.Debug("CreateCustomVolumeFromCopy started")
	defer logger.Debug("CreateCustomVolumeFromCopy finished")

	// Setup the source pool backend instance.
	var srcPool *lxdBackend
	if b.name == srcPoolName {
		srcPool = b // Source and target are in the same pool so share pool var.
	} else {
		// Source is in a different pool to target, so load the pool.
		tmpPool, err := GetPoolByName(b.state, srcPoolName)
		if err != nil {
			return err
		}

		// Convert to lxdBackend so we can access driver.
		tmpBackend, ok := tmpPool.(*lxdBackend)
		if !ok {
			return fmt.Errorf("Pool is not an lxdBackend")
		}

		srcPool = tmpBackend
	}

	// Check source volume exists and is custom type.
	_, srcVolRow, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", srcVolName, db.StoragePoolVolumeTypeCustom, srcPool.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Source volume doesn't exist")
		}

		return err
	}

	// Use the source volume's config if not supplied.
	if config == nil {
		config = srcVolRow.Config
	}

	// Use the source volume's description if not supplied.
	if desc == "" {
		desc = srcVolRow.Description
	}

	// If we are copying snapshots, retrieve a list of snapshots from source volume.
	snapshotNames := []string{}
	if !srcVolOnly {
		snapshots, err := VolumeSnapshotsGet(b.state, srcPoolName, srcVolName, db.StoragePoolVolumeTypeCustom)
		if err != nil {
			return err
		}

		for _, snapshot := range snapshots {
			_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(snapshot.Name)
			snapshotNames = append(snapshotNames, snapShotName)
		}
	}

	// If the source and target are in the same pool then use CreateVolumeFromCopy rather than
	// migration system as it will be quicker.
	if srcPool == b {
		logger.Debug("CreateCustomVolumeFromCopy same-pool mode detected")

		// Create slice to record DB volumes created if revert needed later.
		revertDBVolumes := []string{}
		defer func() {
			// Remove any DB volume rows created if we are reverting.
			for _, volName := range revertDBVolumes {
				b.state.Cluster.StoragePoolVolumeDelete("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
			}
		}()

		vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, config)
		srcVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, srcVolName, srcVolRow.Config)

		// Check the supplied config and remove any fields not relevant for pool type.
		err := b.driver.ValidateVolume(vol, true)
		if err != nil {
			return err
		}

		// Create database entry for new storage volume.
		err = VolumeDBCreate(b.state, "default", b.name, volName, desc, db.StoragePoolVolumeTypeNameCustom, false, vol.Config())
		if err != nil {
			return err
		}

		revertDBVolumes = append(revertDBVolumes, volName)

		if len(snapshotNames) > 0 {
			for _, snapName := range snapshotNames {
				newSnapshotName := drivers.GetSnapshotVolumeName(volName, snapName)

				// Create database entry for new storage volume snapshot.
				err = VolumeDBCreate(b.state, "default", b.name, newSnapshotName, desc, db.StoragePoolVolumeTypeNameCustom, true, vol.Config())
				if err != nil {
					return err
				}

				revertDBVolumes = append(revertDBVolumes, newSnapshotName)
			}
		}

		err = b.driver.CreateVolumeFromCopy(vol, srcVol, !srcVolOnly, op)
		if err != nil {
			return err
		}

		revertDBVolumes = nil
		return nil
	}

	// We are copying volumes between storage pools so use migration system as it will be able
	// to negotiate a common transfer method between pool types.
	logger.Debug("CreateCustomVolumeFromCopy cross-pool mode detected")

	// Use in-memory pipe pair to simulate a connection between the sender and receiver.
	aEnd, bEnd := memorypipe.NewPipePair()

	// Negotiate the migration type to use.
	offeredTypes := srcPool.MigrationTypes(drivers.ContentTypeFS, false)
	offerHeader := migration.TypesToHeader(offeredTypes...)
	migrationType, err := migration.MatchTypes(offerHeader, migration.MigrationFSType_RSYNC, b.MigrationTypes(drivers.ContentTypeFS, false))
	if err != nil {
		return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
	}

	// Run sender and receiver in separate go routines to prevent deadlocks.
	aEndErrCh := make(chan error, 1)
	bEndErrCh := make(chan error, 1)
	go func() {
		err := srcPool.MigrateCustomVolume(aEnd, &migration.VolumeSourceArgs{
			Name:          srcVolName,
			Snapshots:     snapshotNames,
			MigrationType: migrationType,
			TrackProgress: true, // Do use a progress tracker on sender.
		}, op)

		aEndErrCh <- err
	}()

	go func() {
		err := b.CreateCustomVolumeFromMigration(bEnd, migration.VolumeTargetArgs{
			Name:          volName,
			Description:   desc,
			Config:        config,
			Snapshots:     snapshotNames,
			MigrationType: migrationType,
			TrackProgress: false, // Do not use a progress tracker on receiver.

		}, op)

		bEndErrCh <- err
	}()

	// Capture errors from the sender and receiver from their result channels.
	errs := []error{}
	aEndErr := <-aEndErrCh
	if aEndErr != nil {
		aEnd.Close()
		errs = append(errs, aEndErr)
	}

	bEndErr := <-bEndErrCh
	if bEndErr != nil {
		errs = append(errs, bEndErr)
	}

	if len(errs) > 0 {
		return fmt.Errorf("Create custom volume from copy failed: %v", errs)
	}

	return nil
}

// MigrateCustomVolume sends a volume for migration.
func (b *lxdBackend) MigrateCustomVolume(conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": args.Name, "args": args})
	logger.Debug("MigrateCustomVolume started")
	defer logger.Debug("MigrateCustomVolume finished")

	// Volume config not needed to send a volume so set to nil.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, args.Name, nil)
	err := b.driver.MigrateVolume(vol, conn, args, op)
	if err != nil {
		return err
	}

	return nil
}

// CreateCustomVolumeFromMigration receives a volume being migrated.
func (b *lxdBackend) CreateCustomVolumeFromMigration(conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": args.Name, "args": args})
	logger.Debug("CreateCustomVolumeFromMigration started")
	defer logger.Debug("CreateCustomVolumeFromMigration finished")

	// Create slice to record DB volumes created if revert needed later.
	revertDBVolumes := []string{}
	defer func() {
		// Remove any DB volume rows created if we are reverting.
		for _, volName := range revertDBVolumes {
			b.state.Cluster.StoragePoolVolumeDelete("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	// Check the supplied config and remove any fields not relevant for destination pool type.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, args.Name, args.Config)
	err := b.driver.ValidateVolume(vol, true)
	if err != nil {
		return err
	}

	// Create database entry for new storage volume.
	err = VolumeDBCreate(b.state, "default", b.name, args.Name, args.Description, db.StoragePoolVolumeTypeNameCustom, false, vol.Config())
	if err != nil {
		return err
	}

	revertDBVolumes = append(revertDBVolumes, args.Name)

	if len(args.Snapshots) > 0 {
		for _, snapName := range args.Snapshots {
			newSnapshotName := drivers.GetSnapshotVolumeName(args.Name, snapName)

			// Create database entry for new storage volume snapshot.
			err = VolumeDBCreate(b.state, "default", b.name, newSnapshotName, args.Description, db.StoragePoolVolumeTypeNameCustom, true, vol.Config())
			if err != nil {
				return err
			}

			revertDBVolumes = append(revertDBVolumes, newSnapshotName)
		}
	}

	err = b.driver.CreateVolumeFromMigration(vol, conn, args, nil, op)
	if err != nil {
		conn.Close()
		return err
	}

	revertDBVolumes = nil
	return nil
}

// RenameCustomVolume renames a custom volume and its snapshots.
func (b *lxdBackend) RenameCustomVolume(volName string, newVolName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "newVolName": newVolName})
	logger.Debug("RenameCustomVolume started")
	defer logger.Debug("RenameCustomVolume finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	if shared.IsSnapshot(newVolName) {
		return fmt.Errorf("New volume name cannot be a snapshot")
	}

	type volRevert struct {
		oldName string
		newName string
	}

	// Create slice to record DB volumes renamed if revert needed later.
	revertDBVolumes := []volRevert{}
	defer func() {
		// Remove any DB volume rows created if we are reverting.
		for _, vol := range revertDBVolumes {
			b.state.Cluster.StoragePoolVolumeRename("default", vol.newName, vol.oldName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	// Rename each snapshot to have the new parent volume prefix.
	snapshots, err := VolumeSnapshotsGet(b.state, b.name, volName, db.StoragePoolVolumeTypeCustom)
	if err != nil {
		return err
	}

	for _, srcSnapshot := range snapshots {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot.Name)
		newSnapVolName := drivers.GetSnapshotVolumeName(newVolName, snapName)
		err = b.state.Cluster.StoragePoolVolumeRename("default", srcSnapshot.Name, newSnapVolName, db.StoragePoolVolumeTypeCustom, b.ID())
		if err != nil {
			return err
		}

		revertDBVolumes = append(revertDBVolumes, volRevert{
			newName: newSnapVolName,
			oldName: srcSnapshot.Name,
		})
	}

	err = b.state.Cluster.StoragePoolVolumeRename("default", volName, newVolName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	revertDBVolumes = append(revertDBVolumes, volRevert{
		newName: newVolName,
		oldName: volName,
	})

	// There's no need to pass the config as it's not needed when renaming a
	// volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, nil)

	err = b.driver.RenameVolume(vol, newVolName, op)
	if err != nil {
		return err
	}

	revertDBVolumes = nil
	return nil
}

// detectChangedConfig returns the config that has changed between current and new config maps.
// Also returns a boolean indicating whether all of the changed keys start with "user.".
// Deleted keys will be returned as having an empty string value.
func (b *lxdBackend) detectChangedConfig(curConfig, newConfig map[string]string) (map[string]string, bool) {
	// Diff the configurations.
	changedConfig := make(map[string]string)
	userOnly := true
	for key := range curConfig {
		if curConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			changedConfig[key] = newConfig[key] // Will be empty string on deleted keys.
		}
	}

	for key := range newConfig {
		if curConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			changedConfig[key] = newConfig[key]
		}
	}

	return changedConfig, userOnly
}

// UpdateCustomVolume applies the supplied config to the custom volume.
func (b *lxdBackend) UpdateCustomVolume(volName, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateCustomVolume started")
	defer logger.Debug("UpdateCustomVolume finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	// Validate config.
	newVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, newConfig)
	err := b.driver.ValidateVolume(newVol, false)
	if err != nil {
		return err
	}

	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Volume doesn't exist")
		}

		return err
	}

	// Apply config changes if there are any.
	changedConfig, userOnly := b.detectChangedConfig(curVol.Config, newConfig)
	if len(changedConfig) != 0 {
		// Check that the volume's block.filesystem property isn't being changed.
		if changedConfig["block.filesystem"] != "" {
			return fmt.Errorf("Custom volume 'block.filesystem' property cannot be changed")
		}

		curVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, curVol.Config)
		if !userOnly {
			err = b.driver.UpdateVolume(curVol, changedConfig)
			if err != nil {
				return err
			}
		}
	}

	// Check that security.unmapped and security.shifted aren't set together.
	if shared.IsTrue(newConfig["security.unmapped"]) && shared.IsTrue(newConfig["security.shifted"]) {
		return fmt.Errorf("security.unmapped and security.shifted are mutually exclusive")
	}

	// Confirm that no instances are running when changing shifted state.
	if newConfig["security.shifted"] != curVol.Config["security.shifted"] {
		usingVolume, err := VolumeUsedByInstancesWithProfiles(b.state, b.Name(), volName, db.StoragePoolVolumeTypeNameCustom, true)
		if err != nil {
			return err
		}

		if len(usingVolume) != 0 {
			return fmt.Errorf("Cannot modify shifting with running containers using the volume")
		}
	}

	// Unset idmap keys if volume is unmapped.
	if shared.IsTrue(newConfig["security.unmapped"]) {
		delete(newConfig, "volatile.idmap.last")
		delete(newConfig, "volatile.idmap.next")
	}

	// Update the database if something changed.
	if len(changedConfig) != 0 || newDesc != curVol.Description {
		err = b.state.Cluster.StoragePoolVolumeUpdateByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.ID(), newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateCustomVolumeSnapshot updates the description of a custom volume snapshot.
// Volume config is not allowd to be updated and will return an error.
func (b *lxdBackend) UpdateCustomVolumeSnapshot(volName, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateCustomVolumeSnapshot started")
	defer logger.Debug("UpdateCustomVolumeSnapshot finished")

	if !shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume must be a snapshot")
	}

	return b.updateVolumeDescriptionOnly("default", volName, db.StoragePoolVolumeTypeCustom, newDesc, newConfig)
}

// DeleteCustomVolume removes a custom volume and its snapshots.
func (b *lxdBackend) DeleteCustomVolume(volName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName})
	logger.Debug("DeleteCustomVolume started")
	defer logger.Debug("DeleteCustomVolume finished")

	_, _, isSnap := shared.InstanceGetParentAndSnapshotName(volName)
	if isSnap {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	// Retrieve a list of snapshots.
	snapshots, err := VolumeSnapshotsGet(b.state, b.name, volName, db.StoragePoolVolumeTypeCustom)
	if err != nil {
		return err
	}

	// Remove each snapshot.
	for _, snapshot := range snapshots {
		err = b.DeleteCustomVolumeSnapshot(snapshot.Name, op)
		if err != nil {
			return err
		}
	}

	// There's no need to pass config as it's not needed when deleting a volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, nil)

	// Delete the volume from the storage device. Must come after snapshots are removed.
	err = b.driver.DeleteVolume(vol, op)
	if err != nil {
		return err
	}

	// Finally, remove the volume record from the database.
	err = b.state.Cluster.StoragePoolVolumeDelete("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	return nil
}

// GetCustomVolumeUsage returns the disk space used by the custom volume.
func (b *lxdBackend) GetCustomVolumeUsage(volName string) (int64, error) {
	// There's no need to pass config as it's not needed when getting the volume
	// usage.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, nil)

	return b.driver.GetVolumeUsage(vol)
}

// MountCustomVolume mounts a custom volume.
func (b *lxdBackend) MountCustomVolume(volName string, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName})
	logger.Debug("MountCustomVolume started")
	defer logger.Debug("MountCustomVolume finished")

	_, volume, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return false, err
	}

	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, volume.Config)

	return b.driver.MountVolume(vol, op)
}

// UnmountCustomVolume unmounts a custom volume.
func (b *lxdBackend) UnmountCustomVolume(volName string, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName})
	logger.Debug("UnmountCustomVolume started")
	defer logger.Debug("UnmountCustomVolume finished")

	_, volume, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return false, err
	}

	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, volume.Config)

	return b.driver.UnmountVolume(vol, op)
}

// CreateCustomVolumeSnapshot creates a snapshot of a custom volume.
func (b *lxdBackend) CreateCustomVolumeSnapshot(volName string, newSnapshotName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "newSnapshotName": newSnapshotName})
	logger.Debug("CreateCustomVolumeSnapshot started")
	defer logger.Debug("CreateCustomVolumeSnapshot finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume cannot be snapshot")
	}

	if shared.IsSnapshot(newSnapshotName) {
		return fmt.Errorf("Snapshot name is not a valid snapshot name")
	}

	fullSnapshotName := drivers.GetSnapshotVolumeName(volName, newSnapshotName)

	// Check snapshot volume doesn't exist already.
	_, _, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", fullSnapshotName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != db.ErrNoSuchObject {
		if err != nil {
			return err
		}

		return fmt.Errorf("Snapshot by that name already exists")
	}

	// Load parent volume information and check it exists.
	_, parentVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Parent volume doesn't exist")
		}

		return err
	}

	// Create database entry for new storage volume snapshot.
	err = VolumeDBCreate(b.state, "default", b.name, fullSnapshotName, parentVol.Description, db.StoragePoolVolumeTypeNameCustom, true, parentVol.Config)
	if err != nil {
		return err
	}

	revertDB := true
	defer func() {
		if revertDB {
			b.state.Cluster.StoragePoolVolumeDelete("default", fullSnapshotName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, fullSnapshotName, parentVol.Config)

	// Create the snapshot on the storage device.
	err = b.driver.CreateVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	revertDB = false
	return nil
}

// RenameCustomVolumeSnapshot renames a custom volume.
func (b *lxdBackend) RenameCustomVolumeSnapshot(volName string, newSnapshotName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "newSnapshotName": newSnapshotName})
	logger.Debug("RenameCustomVolumeSnapshot started")
	defer logger.Debug("RenameCustomVolumeSnapshot finished")

	parentName, oldSnapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(volName)
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	if shared.IsSnapshot(newSnapshotName) {
		return fmt.Errorf("Invalid new snapshot name")
	}

	// There's no need to pass config as it's not needed when renaming a volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, nil)

	err := b.driver.RenameVolumeSnapshot(vol, newSnapshotName, op)
	if err != nil {
		return err
	}

	newVolName := drivers.GetSnapshotVolumeName(parentName, newSnapshotName)
	err = b.state.Cluster.StoragePoolVolumeRename("default", volName, newVolName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		// Revert rename.
		newVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, newVolName, nil)
		b.driver.RenameVolumeSnapshot(newVol, oldSnapshotName, op)
		return err
	}

	return nil
}

// DeleteCustomVolumeSnapshot removes a custom volume snapshot.
func (b *lxdBackend) DeleteCustomVolumeSnapshot(volName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName})
	logger.Debug("DeleteCustomVolumeSnapshot started")
	defer logger.Debug("DeleteCustomVolumeSnapshot finished")

	isSnap := shared.IsSnapshot(volName)

	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	// There's no need to pass config as it's not needed when deleting a volume
	// snapshot.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, nil)

	// Delete the snapshot from the storage device.
	// Must come before DB StoragePoolVolumeDelete so that the volume ID is still available.
	err := b.driver.DeleteVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	// Remove the snapshot volume record from the database.
	err = b.state.Cluster.StoragePoolVolumeDelete("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	return nil
}

// RestoreCustomVolume restores a custom volume from a snapshot.
func (b *lxdBackend) RestoreCustomVolume(volName string, snapshotName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"volName": volName, "snapshotName": snapshotName})
	logger.Debug("RestoreCustomVolume started")
	defer logger.Debug("RestoreCustomVolume finished")

	// Sanity checks.
	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume cannot be snapshot")
	}

	if shared.IsSnapshot(snapshotName) {
		return fmt.Errorf("Invalid snapshot name")
	}

	// Check that the volume isn't in use.
	usingVolume, err := VolumeUsedByInstancesWithProfiles(b.state, b.Name(), volName, db.StoragePoolVolumeTypeNameCustom, true)
	if err != nil {
		return err
	}

	if len(usingVolume) != 0 {
		return fmt.Errorf("Cannot restore custom volume used by running instances")
	}

	// Get the volume config.
	_, dbVol, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject("default", volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Volume doesn't exist")
		}

		return err
	}

	err = b.driver.RestoreVolume(b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, dbVol.Config), snapshotName, op)
	if err != nil {
		snapErr, ok := err.(drivers.ErrDeleteSnapshots)
		if ok {
			// We need to delete some snapshots and try again.
			for _, snapName := range snapErr.Snapshots {
				err := b.DeleteCustomVolumeSnapshot(fmt.Sprintf("%s/%s", volName, snapName), op)
				if err != nil {
					return err
				}
			}

			// Now try again.
			err = b.driver.RestoreVolume(b.newVolume(drivers.VolumeTypeCustom, drivers.ContentTypeFS, volName, dbVol.Config), snapshotName, op)
			if err != nil {
				return err
			}
		}

		return err
	}

	return nil
}

func (b *lxdBackend) createStorageStructure(path string) error {
	for _, volType := range b.driver.Info().VolumeTypes {
		for _, name := range drivers.BaseDirectories[volType] {
			path := filepath.Join(path, name)
			err := os.MkdirAll(path, 0711)
			if err != nil && !os.IsExist(err) {
				return errors.Wrapf(err, "Failed to create directory '%s'", path)
			}
		}
	}

	return nil
}

// UpdateInstanceBackupFile writes the instance's config to the backup.yaml file on the storage device.
func (b *lxdBackend) UpdateInstanceBackupFile(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UpdateInstanceBackupFile started")
	defer logger.Debug("UpdateInstanceBackupFile finished")

	// We only write backup files out for actual instances.
	if inst.IsSnapshot() {
		return nil
	}

	// Immediately return if the instance directory doesn't exist yet.
	if !shared.PathExists(inst.Path()) {
		return os.ErrNotExist
	}

	// Generate the YAML.
	ci, _, err := inst.Render()
	if err != nil {
		return errors.Wrap(err, "Failed to render instance metadata")
	}

	snapshots, err := inst.Snapshots()
	if err != nil {
		return errors.Wrap(err, "Failed to get snapshots")
	}

	var sis []*api.InstanceSnapshot

	for _, s := range snapshots {
		si, _, err := s.Render()
		if err != nil {
			return err
		}

		sis = append(sis, si.(*api.InstanceSnapshot))
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	_, volume, err := b.state.Cluster.StoragePoolNodeVolumeGetTypeByProject(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(&backup.InstanceConfig{
		Container: ci.(*api.Instance),
		Snapshots: sis,
		Pool:      &b.db,
		Volume:    volume,
	})
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Prefix(inst.Project(), inst.Name())

	// We don't need to use the volume's config for mounting so set to nil.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Update pool information in the backup.yaml file.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// Write the YAML
		path := filepath.Join(inst.Path(), "backup.yaml")
		f, err := os.Create(path)
		if err != nil {
			return errors.Wrapf(err, "Failed to create file '%s'", path)
		}
		defer f.Close()

		err = f.Chmod(0400)
		if err != nil {
			return err
		}

		err = shared.WriteAll(f, data)
		if err != nil {
			return err
		}

		return nil
	}, op)

	return err
}
