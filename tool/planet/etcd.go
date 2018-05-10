package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	etcd "github.com/coreos/etcd/client"
	"github.com/coreos/go-systemd/dbus"
	"github.com/davecgh/go-spew/spew"
	etcdconf "github.com/gravitational/coordinate/config"
	backup "github.com/gravitational/etcd-backup/lib/etcd"
	"github.com/gravitational/planet/lib/box"
	"github.com/gravitational/trace"
	ps "github.com/mitchellh/go-ps"
	log "github.com/sirupsen/logrus"
)

// etcdPromote promotes running etcd proxy to a full member; does nothing if it's already
// running in proxy mode.
//
// Parameters name, initial cluster and state are ones produced by the 'member add'
// command.
//
// Whether etcd is running in proxy mode is determined by ETCD_PROXY environment variable
// normally set in /etc/container-environment inside planet.
//
// To promote proxy to a member we update ETCD_PROXY to disable proxy mode, wipe out
// its state directory and restart the service, as suggested by etcd docs.
func etcdPromote(name, initialCluster, initialClusterState string) error {
	env, err := box.ReadEnvironment(ContainerEnvironmentFile)
	if err != nil {
		return trace.Wrap(err)
	}

	if env.Get(EnvEtcdProxy) == EtcdProxyOff {
		log.Infof("etcd is not running in proxy mode, nothing to do")
		return nil
	}

	newEnv := map[string]string{
		EnvEtcdProxy:               EtcdProxyOff,
		EnvEtcdMemberName:          name,
		EnvEtcdInitialCluster:      initialCluster,
		EnvEtcdInitialClusterState: initialClusterState,
	}

	log.Infof("updating etcd environment: %v", newEnv)
	for k, v := range newEnv {
		env.Upsert(k, v)
	}

	if err := box.WriteEnvironment(ContainerEnvironmentFile, env); err != nil {
		return trace.Wrap(err)
	}

	out, err := exec.Command("/bin/systemctl", "stop", "etcd").CombinedOutput()
	log.Infof("stopping etcd: %v", string(out))
	if err != nil {
		return trace.Wrap(err, "failed to stop etcd: %v", string(out))
	}

	log.Infof("removing %v", ETCDProxyDir)
	if err := os.RemoveAll(ETCDProxyDir); err != nil && !os.IsNotExist(err) {
		return trace.Wrap(err)
	}

	setupEtcd(&Config{
		Rootfs:    "/",
		EtcdProxy: "off",
	})

	out, err = exec.Command("/bin/systemctl", "daemon-reload").CombinedOutput()
	log.Infof("systemctl daemon-reload: %v", string(out))
	if err != nil {
		return trace.Wrap(err, "failed to trigger systemctl daemon-reload: %v", string(out))
	}

	out, err = exec.Command("/bin/systemctl", "start", ETCDServiceName).CombinedOutput()
	log.Infof("starting etcd: %v", string(out))
	if err != nil {
		return trace.Wrap(err, "failed to start etcd: %v", string(out))
	}

	out, err = exec.Command("/bin/systemctl", "restart", PlanetAgentServiceName).CombinedOutput()
	log.Infof("restarting planet-agent: %v", string(out))
	if err != nil {
		return trace.Wrap(err, "failed to restart planet-agent: %v", string(out))
	}

	return nil
}

// etcdInit detects which version of etcd should be running, and sets symlinks to point
// to the correct version
func etcdInit() error {
	desiredVersion, _, err := readEtcdVersion(DefaultPlanetReleaseFile)
	if err != nil {
		return trace.Wrap(err)
	}
	log.Info("Desired etcd version: ", desiredVersion)

	currentVersion, _, err := readEtcdVersion(DefaultEtcdCurrentVersionFile)
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
		currentVersion = AssumeEtcdVersion

		// if the etcd data directory doesn't exist, treat this as a new installation
		if _, err := os.Stat("/ext/etcd/member"); os.IsNotExist(err) {
			// If the etcd data directory doesn't exist, we can assume this
			// is a new install of etcd, and use the latest version.
			log.Info("New installation detected, using etcd version: ", desiredVersion)
			err = writeEtcdEnvironment(DefaultEtcdCurrentVersionFile, desiredVersion, "")
			if err != nil {
				return trace.Wrap(err)
			}
			currentVersion = desiredVersion
		}
	}
	log.Info("Current etcd version: ", currentVersion)

	// symlink /usr/bin/etcd to the version we expect to be running
	for _, path := range []string{"/usr/bin/etcd", "/usr/bin/etcdctl"} {
		// ignore the error from os.Remove, since we don't care if it fails
		_ = os.Remove(path)
		err = os.Symlink(
			fmt.Sprint(path, "-", currentVersion),
			path,
		)
		if err != nil {
			return trace.ConvertSystemError(err)
		}
	}

	// create a symlink for the etcd data
	// this way we can easily support upgrade/rollback by simply changing
	// the pointer to where the data lives
	// Note: in order to support rollback to version 2.3.8, we need to link
	// to /ext/data
	latestDir := path.Join(DefaultEtcdStoreBase, "latest")
	_ = os.Remove(latestDir)
	dest := getBaseEtcdDir(currentVersion)
	err = os.MkdirAll(dest, 700)
	if err != nil && !os.IsExist(err) {
		return trace.ConvertSystemError(err)
	}

	// chown the destination directory to the planet user
	fi, err := os.Stat(DefaultEtcdStoreBase)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	stat := fi.Sys().(*syscall.Stat_t)
	uid := int(stat.Uid)
	gid := int(stat.Gid)
	err = chownDir(dest, uid, gid)
	if err != nil {
		return trace.Wrap(err)
	}

	err = os.Symlink(
		dest,
		latestDir,
	)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func etcdBackup(backupFile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), EtcdUpgradeTimeout)
	defer cancel()

	// If a backup from a previous upgrade exists, clean it up
	if _, err := os.Stat(backupFile); err == nil {
		err = os.Remove(backupFile)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	backupConf := backup.BackupConfig{
		EtcdConfig: etcdconf.Config{
			Endpoints: []string{DefaultEtcdEndpoints},
			KeyFile:   DefaultEtcdctlKeyFile,
			CertFile:  DefaultEtcdctlCertFile,
			CAFile:    DefaultEtcdctlCAFile,
		},
		Prefix: []string{"/"}, // Backup all etcd data
		File:   backupFile,
	}
	log.Info("BackupConfig: ", spew.Sdump(backupConf))
	backupConf.Log = log.StandardLogger()

	err := backup.Backup(ctx, backupConf)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// etcdDisable disables etcd on this machine
// Used during upgrades
func etcdDisable(upgradeService bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), EtcdUpgradeTimeout)
	defer cancel()

	if upgradeService {
		return trace.Wrap(disableService(ctx, ETCDUpgradeServiceName))
	}
	return trace.Wrap(disableService(ctx, ETCDServiceName))
}

// etcdEnable enables a disabled etcd node
func etcdEnable(upgradeService bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), EtcdUpgradeTimeout)
	defer cancel()

	if !upgradeService {
		return trace.Wrap(enableService(ctx, ETCDServiceName))
	}
	// don't actually enable the service if this is a proxy
	env, err := box.ReadEnvironment(ContainerEnvironmentFile)
	if err != nil {
		return trace.Wrap(err)
	}

	if env.Get(EnvEtcdProxy) == EtcdProxyOn {
		log.Infof("etcd is in proxy mode, nothing to do")
		return nil
	}
	return trace.Wrap(enableService(ctx, ETCDUpgradeServiceName))
}

// etcdUpgrade upgrades / rollbacks the etcd upgrade
// the procedure is basically the same for an upgrade or rollback, just with some paths reversed
func etcdUpgrade(rollback bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), EtcdUpgradeTimeout)
	defer cancel()
	log.Info("Updating etcd")

	env, err := box.ReadEnvironment(ContainerEnvironmentFile)
	if err != nil {
		return trace.Wrap(err)
	}

	if env.Get(EnvEtcdProxy) == EtcdProxyOn {
		log.Info("etcd is in proxy mode, nothing to do")
		return nil
	}

	log.Info("Checking etcd service status")
	services := []string{ETCDServiceName, ETCDUpgradeServiceName}
	for _, service := range services {
		status, err := getServiceStatus(service)
		if err != nil {
			log.Warnf("Failed to query status of service %v. Continuing upgrade. Error: %v", service, err)
			continue
		}
		log.Info("%v service status: %v", service, status)
		if status != "inactive" && status != "failed" {
			return trace.BadParameter("%v must be disabled in order to run the upgrade. current status: %v", service, status)
		}
	}

	// In order to upgrade in a re-entrant way
	// we need to make sure that if the upgrade or rollback is repeated
	// that it skips anything that has been done on a previous run, and continues anything that may have failed
	desiredVersion, _, err := readEtcdVersion(DefaultPlanetReleaseFile)
	if err != nil {
		return trace.Wrap(err)
	}
	log.Info("Desired etcd version: ", desiredVersion)

	currentVersion, backupVersion, err := readEtcdVersion(DefaultEtcdCurrentVersionFile)
	if err != nil {
		if trace.IsNotFound(err) {
			currentVersion = AssumeEtcdVersion
		} else {
			return trace.Wrap(err)
		}
	}
	log.Info("Current etcd version: ", currentVersion)
	log.Info("Backup etcd version: ", backupVersion)

	if rollback {
		// in order to rollback, write the backup version as the current version, with no backup version
		if backupVersion != "" {
			err = writeEtcdEnvironment(DefaultEtcdCurrentVersionFile, backupVersion, "")
			if err != nil {
				return trace.Wrap(err)
			}
		}
	} else {
		// in order to upgrade, write the new version to to disk with the current version as backup
		// if current version == desired version, we must have already run this step
		if currentVersion != desiredVersion {
			err = writeEtcdEnvironment(DefaultEtcdCurrentVersionFile, desiredVersion, currentVersion)
			if err != nil {
				return trace.Wrap(err)
			}

			// wipe old backups leftover from previous upgrades
			// Note: if this fails, but previous steps were successfull, the backups won't get cleaned up
			if backupVersion != "" {
				path := path.Join(getBaseEtcdDir(backupVersion), "member")
				err = os.RemoveAll(path)
				if err != nil {
					return trace.ConvertSystemError(err)
				}
			}
		}

		// wipe data directory of any previous upgrade attempt
		path := path.Join(getBaseEtcdDir(desiredVersion), "member")
		err = os.RemoveAll(path)
		if err != nil && !os.IsNotExist(err) {
			return trace.ConvertSystemError(err)
		}
	}

	// reset the kubernetes api server to take advantage of any new etcd settings that may have changed
	// this only happens if the service is already running
	status, err := getServiceStatus(APIServerServiceName)
	if err != nil {
		return trace.Wrap(err)
	}
	if status != "inactive" {
		tryResetService(ctx, APIServerServiceName)
	}

	log.Info("Upgrade complete")

	return nil
}

func getBaseEtcdDir(version string) string {
	p := DefaultEtcdStoreBase
	if version != AssumeEtcdVersion {
		p = path.Join(DefaultEtcdStoreBase, version)
	}
	return p
}

func etcdRestore(file string) error {
	ctx := context.TODO()
	log.Info("Restoring backup to temporary etcd")
	restoreConf := backup.RestoreConfig{
		EtcdConfig: etcdconf.Config{
			Endpoints: []string{DefaultEtcdUpgradeEndpoints},
			KeyFile:   DefaultEtcdctlKeyFile,
			CertFile:  DefaultEtcdctlCertFile,
			CAFile:    DefaultEtcdctlCAFile,
		},
		Prefix:        []string{"/"},         // Restore all etcd data
		MigratePrefix: []string{"/registry"}, // migrate kubernetes data to etcd3 datastore
		File:          file,
	}
	log.Info("RestoreConfig: ", spew.Sdump(restoreConf))
	restoreConf.Log = log.StandardLogger()

	err := backup.Restore(ctx, restoreConf)
	if err != nil {
		return trace.Wrap(err)
	}

	log.Info("Restore complete")
	return nil
}

func convertError(err error) error {
	if err == nil {
		return nil
	}
	switch err := err.(type) {
	case *etcd.ClusterError:
		return trace.Wrap(err, err.Detail())
	case etcd.Error:
		switch err.Code {
		case etcd.ErrorCodeKeyNotFound:
			return trace.NotFound(err.Error())
		case etcd.ErrorCodeNotFile:
			return trace.BadParameter(err.Error())
		case etcd.ErrorCodeNodeExist:
			return trace.AlreadyExists(err.Error())
		case etcd.ErrorCodeTestFailed:
			return trace.CompareFailed(err.Error())
		}
	}
	return err
}

// systemctl runs a local systemctl command.
// TODO(knisbet): I'm using systemctl here, because using go-systemd and dbus appears to be unreliable, with
// masking unit files not working. Ideally, this will use dbus at some point in the future.
func systemctl(ctx context.Context, operation, service string) error {
	out, err := exec.CommandContext(ctx, "/bin/systemctl", "--no-block", operation, service).CombinedOutput()
	log.Infof("%v %v: %v", operation, service, string(out))
	if err != nil {
		return trace.Wrap(err, "failed to %v %v: %v", operation, service, string(out))
	}
	return nil
}

// waitForEtcdStopped waits for etcd to not be present in the process list
// the problem is, when shutting down etcd, systemd will respond when the process has been told to shutdown
// but this leaves a race, where we might be continuing while etcd is still cleanly shutting down
func waitForEtcdStopped(ctx context.Context) error {
	tick := time.Tick(WaitInterval)
loop:
	for {
		select {
		case <-ctx.Done():
			return trace.Wrap(ctx.Err())
		case <-tick:
		}

		procs, err := ps.Processes()
		if err != nil {
			return trace.Wrap(err)
		}
		for _, proc := range procs {
			if proc.Executable() == "etcd" {
				continue loop
			}
		}
		return nil
	}
}

// tryResetService will request for systemd to restart a system service
func tryResetService(ctx context.Context, service string) {
	// ignoring error results is intentional
	err := systemctl(ctx, "restart", service)
	if err != nil {
		log.Warn("error attempting to restart service", err)
	}
}

func disableService(ctx context.Context, service string) error {
	err := systemctl(ctx, "mask", service)
	if err != nil {
		return trace.Wrap(err)
	}
	err = systemctl(ctx, "stop", service)
	if err != nil {
		return trace.Wrap(err)
	}
	return trace.Wrap(waitForEtcdStopped(ctx))
}

func enableService(ctx context.Context, service string) error {
	err := systemctl(ctx, "unmask", service)
	if err != nil {
		return trace.Wrap(err)
	}
	return trace.Wrap(systemctl(ctx, "start", service))
}

func getServiceStatus(service string) (string, error) {
	conn, err := dbus.New()
	if err != nil {
		return "", trace.Wrap(err)
	}

	status, err := conn.ListUnitsByNames([]string{service})
	if err != nil {
		return "", trace.Wrap(err)
	}
	if len(status) != 1 {
		return "", trace.BadParameter("unexpected number of status results when checking service '%q'", service)
	}

	return status[0].ActiveState, nil
}

func readEtcdVersion(path string) (currentVersion string, prevVersion string, err error) {
	inFile, err := os.Open(path)
	if err != nil {
		return "", "", trace.ConvertSystemError(err)
	}
	defer inFile.Close()

	scanner := bufio.NewScanner(inFile)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.Contains(line, "=") {
			split := strings.SplitN(line, "=", 2)
			if len(split) == 2 {
				switch split[0] {
				case EnvEtcdVersion:
					currentVersion = split[1]
				case EnvEtcdPrevVersion:
					prevVersion = split[1]
				}
			}
		}
	}

	if currentVersion == "" {
		return "", "", trace.BadParameter("unable to parse etcd version")
	}
	return currentVersion, prevVersion, nil
}

func writeEtcdEnvironment(path string, version string, prevVersion string) error {
	err := os.MkdirAll(filepath.Dir(path), 644)
	if err != nil {
		return trace.ConvertSystemError(err)
	}

	f, err := os.Create(path)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	defer f.Close()

	_, err = fmt.Fprint(f, EnvEtcdVersion, "=", version, "\n")
	if err != nil {
		return err
	}

	if prevVersion != "" {
		_, err = fmt.Fprint(f, EnvEtcdPrevVersion, "=", prevVersion, "\n")
		if err != nil {
			return err
		}
	}

	backend := "etcd3"
	if version == AssumeEtcdVersion {
		backend = "etcd2"
	}
	_, err = fmt.Fprint(f, EnvStorageBackend, "=", backend, "\n")
	if err != nil {
		return err
	}

	return nil
}
