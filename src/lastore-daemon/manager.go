/*
 * Copyright (C) 2015 ~ 2017 Deepin Technology Co., Ltd.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus"
	apps "github.com/linuxdeepin/go-dbus-factory/com.deepin.daemon.apps"

	"internal/system"
	"internal/utils"

	"pkg.deepin.io/lib/dbusutil"
	"pkg.deepin.io/lib/keyfile"
	"pkg.deepin.io/lib/procfs"
	"pkg.deepin.io/lib/strv"

	log "github.com/cihub/seelog"
)

const (
	UserExperServiceName = "com.deepin.userexperience.Daemon"
	UserExperPath        = "/com/deepin/userexperience/Daemon"

	UserExperInstallApp   = "installapp"
	UserExperUninstallApp = "uninstallapp"

	uosReleaseNotePkgName = "uos-release-note"

	appStoreDaemonPath    = "/usr/bin/deepin-app-store-daemon"
	oldAppStoreDaemonPath = "/usr/bin/deepin-appstore-daemon"
	printerPath           = "/usr/bin/dde-printer"
	printerHelperPath     = "/usr/bin/dde-printer-helper"
	sessionDaemonPath     = "/usr/lib/deepin-daemon/dde-session-daemon"
	langSelectorPath      = "/usr/lib/deepin-daemon/langselector"
	controlCenterPath     = "/usr/bin/dde-control-center"
	controlCenterCmdLine  = "/usr/share/applications/dde-control-center.deskto" // 缺个 p 是因为 deepin-turbo 修改命令的时候 buffer 不够用, 所以截断了.
)

var (
	allowInstallPackageExecPaths = strv.Strv{
		appStoreDaemonPath,
		oldAppStoreDaemonPath,
		printerPath,
		printerHelperPath,
		langSelectorPath,
	}
	allowRemovePackageExecPaths = strv.Strv{
		appStoreDaemonPath,
		oldAppStoreDaemonPath,
		sessionDaemonPath,
		langSelectorPath,
	}
)

const MaxCacheSize = 500.0 //size MB

type Manager struct {
	service *dbusutil.Service
	do      sync.Mutex
	b       system.System
	config  *Config

	PropsMu sync.RWMutex
	// dbusutil-gen: equal=nil
	JobList    []dbus.ObjectPath
	jobList    []*Job
	jobManager *JobManager
	updater    *Updater

	// dbusutil-gen: ignore
	SystemArchitectures []system.Architecture

	// dbusutil-gen: equal=nil
	UpgradableApps []string

	SystemOnChanging   bool
	AutoClean          bool
	autoCleanCfgChange chan struct{}

	inhibitFd        dbus.UnixFD
	updateSourceOnce bool

	apps apps.Apps

	UpdateMode uint64 `prop:"access:rw"`

	isUpdateSucceed bool
}

/*
NOTE: Most of export function of Manager will hold the lock,
so don't invoke they in inner functions
*/

func NewManager(service *dbusutil.Service, b system.System, c *Config) *Manager {
	archs, err := system.SystemArchitectures()
	if err != nil {
		_ = log.Errorf("Can't detect system supported architectures %v\n", err)
		return nil
	}

	m := &Manager{
		service:             service,
		config:              c,
		b:                   b,
		SystemArchitectures: archs,
		inhibitFd:           -1,
		AutoClean:           c.AutoClean,
		UpdateMode:          c.UpdateMode,
	}
	sysBus := service.Conn()
	m.apps = apps.NewApps(sysBus)

	m.jobManager = NewJobManager(service, b, m.updateJobList)
	go m.jobManager.Dispatch()

	m.updateJobList()

	go m.loopCheck()
	return m
}

var pkgNameRegexp = regexp.MustCompile(`^[a-z0-9]`)

func NormalizePackageNames(s string) ([]string, error) {
	pkgNames := strings.Fields(s)
	for _, pkgName := range pkgNames {
		if !pkgNameRegexp.MatchString(pkgName) {
			return nil, fmt.Errorf("invalid package name %q", pkgName)
		}
	}

	if s == "" || len(pkgNames) == 0 {
		return nil, fmt.Errorf("Empty value")
	}
	return pkgNames, nil
}

func makeEnvironWithSender(service *dbusutil.Service, sender dbus.Sender) (map[string]string, error) {
	environ := make(map[string]string)

	pid, err := service.GetConnPID(string(sender))
	if err != nil {
		return nil, err
	}

	p := procfs.Process(pid)
	envVars, err := p.Environ()
	if err != nil {
		_ = log.Warnf("failed to get process %d environ: %v", p, err)
	} else {
		environ["DISPLAY"] = envVars.Get("DISPLAY")
		environ["XAUTHORITY"] = envVars.Get("XAUTHORITY")
		environ["DEEPIN_LASTORE_LANG"] = getLang(envVars)
	}
	return environ, nil
}

func getUsedLang(environ map[string]string) string {
	return environ["DEEPIN_LASTORE_LANG"]
}

func getLang(envVars procfs.EnvVars) string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGE", "LANG"} {
		value := envVars.Get(name)
		if value != "" {
			return value
		}
	}
	return ""
}

// execPath和cmdLine可以有一个为空,其中一个存在即可作为判断调用者的依据
func (m *Manager) getExecutablePathAndCmdline(sender dbus.Sender) (string, string, error) {
	pid, err := m.service.GetConnPID(string(sender))
	if err != nil {
		return "", "", err
	}

	proc := procfs.Process(pid)

	execPath, err := proc.Exe()
	if err != nil {
		// 当调用者在使用过程中发生了更新,则在获取该进程的exe时,会出现lstat xxx (deleted)此类的error,如果发生的是覆盖,则该路径依旧存在,因此增加以下判断
		pErr, ok := err.(*os.PathError)
		if ok {
			if os.IsNotExist(pErr.Err) {
				errExecPath := strings.Replace(pErr.Path, "(deleted)", "", -1)
				oldExecPath := strings.TrimSpace(errExecPath)
				if system.NormalFileExists(oldExecPath) {
					execPath = oldExecPath
					err = nil
				}
			}
		}
	}

	cmdLine, err1 := proc.Cmdline()
	if err != nil && err1 != nil {
		return "", "", errors.New(strings.Join([]string{
			err.Error(),
			err1.Error(),
		}, ";"))
	}
	return execPath, strings.Join(cmdLine, " "), nil
}

func (m *Manager) updatePackage(sender dbus.Sender, jobName string, packages string) (*Job, error) {
	pkgs, err := NormalizePackageNames(packages)
	if err != nil {
		return nil, fmt.Errorf("invalid packages arguments %q : %v", packages, err)
	}

	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return nil, dbusutil.ToError(err)
	}
	caller := mapMethodCaller(execPath, cmdLine)
	m.ensureUpdateSourceOnce(caller)
	environ, err := makeEnvironWithSender(m.service, sender)
	if err != nil {
		return nil, err
	}

	m.do.Lock()
	job, err := m.jobManager.CreateJob(jobName, system.UpdateJobType, pkgs, environ, 0)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("UpdatePackage %q error: %v\n", packages, err)
	}
	job.caller = caller
	return job, err
}

func (m *Manager) UpdatePackage(sender dbus.Sender, jobName string, packages string) (job dbus.ObjectPath,
	busErr *dbus.Error) {
	jobObj, err := m.updatePackage(sender, jobName, packages)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	return jobObj.getPath(), nil
}

func (m *Manager) installPackage(sender dbus.Sender, jobName string, packages string) (*Job, error) {
	pkgs, err := NormalizePackageNames(packages)
	if err != nil {
		return nil, fmt.Errorf("invalid packages arguments %q : %v", packages, err)
	}

	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return nil, dbusutil.ToError(err)
	}
	caller := mapMethodCaller(execPath, cmdLine)
	m.ensureUpdateSourceOnce(caller)
	environ, err := makeEnvironWithSender(m.service, sender)
	if err != nil {
		return nil, err
	}

	lang := getUsedLang(environ)
	if lang == "" {
		_ = log.Warn("failed to get lang")
		return m.installPkg(jobName, packages, environ)
	}

	localePkgs := QueryEnhancedLocalePackages(system.QueryPackageInstallable, caller == methodCallerControlCenter, lang, pkgs...)
	if len(localePkgs) != 0 {
		log.Infof("Follow locale packages will be installed:%v\n", localePkgs)
	}

	pkgs = append(pkgs, localePkgs...)
	return m.installPkg(jobName, strings.Join(pkgs, " "), environ)
}

func (m *Manager) InstallPackage(sender dbus.Sender, jobName string, packages string) (job dbus.ObjectPath,
	busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	uid, err := m.service.GetConnUID(string(sender))
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}
	if !allowInstallPackageExecPaths.Contains(execPath) &&
		uid != 0 {
		err = fmt.Errorf("%q is not allowed to install packages", execPath)
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	jobObj, err := m.installPackage(sender, jobName, packages)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	jobObj.next.caller = mapMethodCaller(execPath, cmdLine)
	return jobObj.getPath(), nil
}

func sendInstallMsgToUserExperModule(msg, path, name, id string) {
	bus, err := dbus.SystemBus()
	if err != nil {
		_ = log.Warn(err)
		return
	}
	userexp := bus.Object(UserExperServiceName, UserExperPath)
	ctx, cancelFn := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFn()
	// 设置两秒的超时，如果两秒内函数没处理完，则返回err，并且不会阻塞
	err = userexp.CallWithContext(ctx, UserExperServiceName+".SendAppInstallData", 0, msg, path, name, id).Err
	if err != nil {
		_ = log.Warnf("failed to call %s.SendAppInstallData, %v", UserExperServiceName, err)
	} else {
		log.Debugf("send %s message to ue module", msg)
	}
}

func (m *Manager) installPkg(jobName, packages string, environ map[string]string) (*Job, error) {
	pList := strings.Fields(packages)

	m.do.Lock()
	job, err := m.jobManager.CreateJob(jobName, system.InstallJobType, pList, environ, 0)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("installPackage %q error: %v\n", packages, err)
	}

	if job != nil && !isCommunity() {
		job.setHooks(map[string]func(){
			string(system.SucceedStatus): func() {
				for _, pkg := range job.Packages {
					log.Debugf("install app %s success, notify ue module", pkg)
					sendInstallMsgToUserExperModule(UserExperInstallApp, "", jobName, pkg)
				}
			},
		})
	}

	return job, err
}

// dont collect experience message if edition is community
func isCommunity() bool {
	kf := keyfile.NewKeyFile()
	err := kf.LoadFromFile("/etc/os-version")
	// 为避免收集数据的风险，读不到此文件，或者Edition文件不存在也不收集数据
	if err != nil {
		return true
	}
	edition, err := kf.GetString("Version", "EditionName")
	if err != nil {
		return true
	}
	if edition == "Community" {
		return true
	}
	return false
}

func listPackageDesktopFiles(pkg string) []string {
	var result []string
	filenames := system.ListPackageFile(pkg)
	for _, filename := range filenames {
		if strings.HasPrefix(filename, "/usr/") {
			// len /usr/ is 5
			if strings.HasSuffix(filename, ".desktop") &&
				(strings.HasPrefix(filename[5:], "share/applications") ||
					strings.HasPrefix(filename[5:], "local/share/applications")) {

				fileInfo, err := os.Stat(filename)
				if err != nil {
					continue
				}
				if fileInfo.IsDir() {
					continue
				}
				if !utf8.ValidString(filename) {
					continue
				}
				result = append(result, filename)
			}
		}
	}
	return result
}

func (m *Manager) removePackage(sender dbus.Sender, jobName string, packages string) (*Job, error) {
	pkgs, err := NormalizePackageNames(packages)
	if err != nil {
		return nil, fmt.Errorf("invalid packages arguments %q : %v", packages, err)
	}

	if len(pkgs) == 1 {
		desktopFiles := listPackageDesktopFiles(pkgs[0])
		if len(desktopFiles) > 0 {
			err = m.apps.LaunchedRecorder().UninstallHints(0, desktopFiles)
			if err != nil {
				_ = log.Warnf("call UninstallHints(desktopFiles: %v) error: %v",
					desktopFiles, err)
			}
		}
	}

	environ, err := makeEnvironWithSender(m.service, sender)
	if err != nil {
		return nil, err
	}

	m.do.Lock()
	job, err := m.jobManager.CreateJob(jobName, system.RemoveJobType, pkgs, environ, 0)
	m.do.Unlock()

	if job != nil && !isCommunity() {
		job.setHooks(map[string]func(){
			string(system.SucceedStatus): func() {
				for _, pkg := range job.Packages {
					log.Debugf("uninstall app %s success, notify ue module", pkg)
					sendInstallMsgToUserExperModule(UserExperUninstallApp, "", jobName, pkg)
				}
			},
		})
	}

	if err != nil {
		_ = log.Warnf("removePackage %q error: %v\n", packages, err)
	}
	return job, err
}

func (m *Manager) RemovePackage(sender dbus.Sender, jobName string, packages string) (job dbus.ObjectPath,
	busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	uid, err := m.service.GetConnUID(string(sender))
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	if !allowRemovePackageExecPaths.Contains(execPath) &&
		uid != 0 {
		err = fmt.Errorf("%q is not allowed to remove packages", execPath)
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	jobObj, err := m.removePackage(sender, jobName, packages)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	jobObj.caller = mapMethodCaller(execPath, cmdLine)
	return jobObj.getPath(), nil
}

func (m *Manager) ensureUpdateSourceOnce(caller methodCaller) {
	m.PropsMu.Lock()
	updateOnce := m.updateSourceOnce
	m.PropsMu.Unlock()

	if updateOnce {
		return
	}

	_, err := m.updateSource(false, caller)
	if err != nil {
		_ = log.Warn(err)
	}
}

func (m *Manager) handleUpdateInfosChanged() {
	log.Info("handleUpdateInfosChanged")
	info, err := system.SystemUpgradeInfo()
	if err != nil {
		_ = log.Error("failed to get upgrade info:", err)
	}
	m.updater.loadUpdateInfos(info)
	m.updatableApps(info)
	m.PropsMu.Lock()
	isUpdateSucceed := m.isUpdateSucceed
	m.PropsMu.Unlock()
	if m.updater.AutoDownloadUpdates && len(m.updater.UpdatablePackages) > 0 && isUpdateSucceed {
		log.Info("auto download updates")
		go func() {
			_, err := m.prepareDistUpgrade(methodCallerControlCenter) // 自动下载使用控制中心的配置
			if err != nil {
				_ = log.Error("failed to prepare dist-upgrade:", err)
			}
		}()
	}
}

func (m *Manager) updateSource(needNotify bool, caller methodCaller) (*Job, error) {
	m.do.Lock()
	var jobName string
	if needNotify {
		jobName = "+notify"
	}
	var job *Job
	var err error
	switch caller {
	case methodCallerControlCenter:
		job, err = m.jobManager.CreateJob(jobName, system.CustomUpdateJobType, nil, nil, m.UpdateMode)
	default:
		job, err = m.jobManager.CreateJob(jobName, system.UpdateSourceJobType, nil, nil, 0)
	}
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("UpdateSource error: %v\n", err)
	}
	if job != nil {
		m.PropsMu.Lock()
		m.updateSourceOnce = true
		m.isUpdateSucceed = false
		m.PropsMu.Unlock()
		job.setHooks(map[string]func(){
			string(system.SucceedStatus): func() {
				m.PropsMu.Lock()
				m.isUpdateSucceed = true
				m.PropsMu.Unlock()
				go m.installUOSReleaseNote()
			},
			string(system.EndStatus): func() {
				m.handleUpdateInfosChanged()
			},
		})
	}
	return job, err
}

func (m *Manager) UpdateSource(sender dbus.Sender) (job dbus.ObjectPath, busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}
	jobObj, err := m.updateSource(false, mapMethodCaller(execPath, cmdLine))
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}

	_ = m.config.UpdateLastCheckTime()

	return jobObj.getPath(), nil
}

func (m *Manager) cancelAllJob() error {
	var updateJobIds []string
	for _, job := range m.jobManager.List() {
		if job.Type == system.UpdateJobType && job.Status != system.RunningStatus {
			updateJobIds = append(updateJobIds, job.Id)
		}
	}

	for _, jobId := range updateJobIds {
		err := m.jobManager.CleanJob(jobId)
		if err != nil {
			_ = log.Warnf("CleanJob %q error: %v\n", jobId, err)
		}
	}
	return nil
}

func (m *Manager) DistUpgrade(sender dbus.Sender) (job dbus.ObjectPath, busErr *dbus.Error) {
	jobObj, err := m.distUpgrade(sender)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	return jobObj.getPath(), nil
}

func (m *Manager) distUpgrade(sender dbus.Sender) (*Job, error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return nil, dbusutil.ToError(err)
	}
	caller := mapMethodCaller(execPath, cmdLine)
	m.ensureUpdateSourceOnce(caller)
	environ, err := makeEnvironWithSender(m.service, sender)
	if err != nil {
		return nil, err
	}

	m.updateJobList()

	m.PropsMu.RLock()
	upgradableApps := m.UpgradableApps
	m.PropsMu.RUnlock()
	if len(upgradableApps) == 0 {
		return nil, system.NotFoundError("empty UpgradableApps")
	}

	m.do.Lock()
	defer m.do.Unlock()

	job, err := m.jobManager.CreateJob("", system.DistUpgradeJobType, upgradableApps, environ, 0)
	if err != nil {
		_ = log.Warnf("DistUpgrade error: %v\n", err)
		return nil, err
	}
	job.caller = caller
	cancelErr := m.cancelAllJob()
	if cancelErr != nil {
		_ = log.Warn(cancelErr)
	}

	return job, err
}

func (m *Manager) PrepareDistUpgrade(sender dbus.Sender) (job dbus.ObjectPath, busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return "/", dbusutil.ToError(err)
	}
	jobObj, err := m.prepareDistUpgrade(mapMethodCaller(execPath, cmdLine))
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	return jobObj.getPath(), nil
}

func (m *Manager) prepareDistUpgrade(caller methodCaller) (*Job, error) {
	m.ensureUpdateSourceOnce(caller)
	m.updateJobList()

	m.PropsMu.RLock()
	upgradableApps := m.UpgradableApps
	m.PropsMu.RUnlock()

	if len(upgradableApps) == 0 {
		return nil, system.NotFoundError("empty UpgradableApps")
	}
	if s, err := system.QueryPackageDownloadSize(true, upgradableApps...); err == nil && s == 0 {
		return nil, system.NotFoundError("no need download")
	}

	m.do.Lock()
	job, err := m.jobManager.CreateJob("", system.PrepareDistUpgradeJobType, upgradableApps, nil, 0)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("PrepareDistUpgrade error: %v\n", err)
		return nil, err
	}
	return job, err
}

func (m *Manager) StartJob(jobId string) *dbus.Error {
	m.do.Lock()
	err := m.jobManager.MarkStart(jobId)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("StartJob %q error: %v\n", jobId, err)
	}
	return dbusutil.ToError(err)
}

func (m *Manager) PauseJob(jobId string) *dbus.Error {
	m.do.Lock()
	err := m.jobManager.PauseJob(jobId)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("PauseJob %q error: %v\n", jobId, err)
	}
	return dbusutil.ToError(err)
}

func (m *Manager) CleanJob(jobId string) *dbus.Error {
	m.do.Lock()
	err := m.jobManager.CleanJob(jobId)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("CleanJob %q error: %v\n", jobId, err)
	}
	return dbusutil.ToError(err)
}

func (m *Manager) PackagesDownloadSize(sender dbus.Sender, packages []string) (size int64, busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return 0, dbusutil.ToError(err)
	}
	caller := mapMethodCaller(execPath, cmdLine)
	m.ensureUpdateSourceOnce(caller)

	s, err := system.QueryPackageDownloadSize(caller == methodCallerControlCenter, packages...)
	if err != nil || s == system.SizeUnknown {
		_ = log.Warnf("PackagesDownloadSize(%q)=%0.2f %v\n", strings.Join(packages, " "), s, err)
	}
	return int64(s), dbusutil.ToError(err)
}

func (m *Manager) PackageInstallable(sender dbus.Sender, pkgId string) (installable bool, busErr *dbus.Error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return false, dbusutil.ToError(err)
	}
	caller := mapMethodCaller(execPath, cmdLine)
	return system.QueryPackageInstallable(caller == methodCallerControlCenter, pkgId), nil
}

func (m *Manager) PackageExists(pkgId string) (exist bool, busErr *dbus.Error) {
	return system.QueryPackageInstalled(pkgId), nil
}

// TODO: Remove this API
func (m *Manager) PackageDesktopPath(pkgId string) (desktopPath string, busErr *dbus.Error) {
	p, err := utils.RunCommand("/usr/bin/lastore-tools", "querydesktop", pkgId)
	if err != nil {
		_ = log.Warnf("QueryDesktopPath failed: %q\n", err)
		return "", dbusutil.ToError(err)
	}
	return p, nil
}

func (m *Manager) SetRegion(region string) *dbus.Error {
	err := m.config.SetAppstoreRegion(region)
	return dbusutil.ToError(err)
}

func (m *Manager) SetAutoClean(enable bool) *dbus.Error {
	if m.AutoClean == enable {
		return nil
	}

	// save the config to disk
	err := m.config.SetAutoClean(enable)
	if err != nil {
		return dbusutil.ToError(err)
	}

	m.AutoClean = enable
	m.autoCleanCfgChange <- struct{}{}
	err = m.emitPropChangedAutoClean(enable)
	if err != nil {
		_ = log.Warn(err)
	}
	return nil
}

func (m *Manager) GetArchivesInfo() (info string, busErr *dbus.Error) {
	info, err := getArchiveInfo()
	if err != nil {
		return "", dbusutil.ToError(err)
	}
	return info, nil
}

func getArchiveInfo() (string, error) {
	out, err := exec.Command("/usr/bin/lastore-apt-clean", "-print-json").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (m *Manager) CleanArchives() (job dbus.ObjectPath, busErr *dbus.Error) {
	jobObj, err := m.cleanArchives(false)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	return jobObj.getPath(), nil
}

func (m *Manager) cleanArchives(needNotify bool) (*Job, error) {
	var jobName string
	if needNotify {
		jobName = "+notify"
	}

	m.do.Lock()
	job, err := m.jobManager.CreateJob(jobName, system.CleanJobType, nil, nil, 0)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("CleanArchives error: %v", err)
		return nil, err
	}

	err = m.config.UpdateLastCleanTime()
	if err != nil {
		return nil, err
	}
	err = m.config.UpdateLastCheckCacheSizeTime()
	if err != nil {
		return nil, err
	}

	return job, err
}

func (m *Manager) loopCheck() {
	m.autoCleanCfgChange = make(chan struct{})
	const checkInterval = time.Second * 600

	doClean := func() {
		log.Debug("call doClean")

		_, err := m.cleanArchives(true)
		if err != nil {
			_ = log.Warnf("CleanArchives failed: %v", err)
		}
	}

	calcRemainingDuration := func() time.Duration {
		elapsed := time.Since(m.config.LastCleanTime)
		if elapsed < 0 {
			// now time < last clean time : last clean time (from config) is invalid
			return -1
		}
		return m.config.CleanInterval - elapsed
	}

	calcRemainingCleanCacheOverLimitDuration := func() time.Duration {
		elapsed := time.Since(m.config.LastCheckCacheSizeTime)
		if elapsed < 0 {
			// now time < last check cache size time : last check cache size time (from config) is invalid
			return -1
		}
		return m.config.CleanIntervalCacheOverLimit - elapsed
	}

	for {
		select {
		case <-m.autoCleanCfgChange:
			log.Debug("auto clean config changed")
			continue
		case <-time.After(checkInterval):
			if m.AutoClean {
				remaining := calcRemainingDuration()
				log.Debugf("auto clean remaining duration: %v", remaining)
				if remaining < 0 {
					doClean()
					continue
				}

				aptCachePath, _ := system.GetArchivesDir(system.LastoreAptOrgConfPath)
				aptCacheSize, _ := system.QueryFileCacheSize(aptCachePath)
				lastoreCachePath, _ := system.GetArchivesDir(system.LastoreAptV2ConfPath)
				lastoreCacheSize, _ := system.QueryFileCacheSize(lastoreCachePath)
				aptCacheSize = aptCacheSize / 1024.0 // kb to mb
				lastoreCacheSize = lastoreCacheSize / 1024.0
				cacheSize := aptCacheSize + lastoreCacheSize
				if cacheSize > MaxCacheSize {
					remainingCleanCacheOverLimitDuration := calcRemainingCleanCacheOverLimitDuration()
					log.Debugf("clean cache over limit remaining duration: %v", remainingCleanCacheOverLimitDuration)
					if remainingCleanCacheOverLimitDuration < 0 {
						doClean()
					}
				}
			} else {
				log.Debug("auto clean disabled")
			}
		}
	}
}

func (m *Manager) FixError(sender dbus.Sender, errType string) (job dbus.ObjectPath, busErr *dbus.Error) {
	jobObj, err := m.fixError(sender, errType)
	if err != nil {
		return "/", dbusutil.ToError(err)
	}
	return jobObj.getPath(), nil
}

func (m *Manager) fixError(sender dbus.Sender, errType string) (*Job, error) {
	execPath, cmdLine, err := m.getExecutablePathAndCmdline(sender)
	if err != nil {
		_ = log.Warn(err)
		return nil, dbusutil.ToError(err)
	}
	m.ensureUpdateSourceOnce(mapMethodCaller(execPath, cmdLine))
	environ, err := makeEnvironWithSender(m.service, sender)
	if err != nil {
		return nil, err
	}

	switch errType {
	case system.ErrTypeDpkgInterrupted, system.ErrTypeDependenciesBroken:
		// good error type
	default:
		return nil, errors.New("invalid error type")
	}

	m.do.Lock()
	job, err := m.jobManager.CreateJob("", system.FixErrorJobType,
		[]string{errType}, environ, 0)
	m.do.Unlock()

	if err != nil {
		_ = log.Warnf("fixError error: %v", err)
		return nil, err
	}
	return job, err
}

func (m *Manager) installUOSReleaseNote() {
	log.Info("installUOSReleaseNote begin")
	bExists, _ := m.PackageExists(uosReleaseNotePkgName)
	if bExists {
		for _, v := range m.updater.UpdatablePackages {
			if v == uosReleaseNotePkgName {
				_, err := m.installPkg("", uosReleaseNotePkgName, nil)
				if err != nil {
					_ = log.Warn(err)
				}
				break
			}
		}
	} else {
		bInstalled := system.QueryPackageInstallable(true, uosReleaseNotePkgName)
		if bInstalled {
			_, err := m.installPkg("", uosReleaseNotePkgName, nil)
			if err != nil {
				_ = log.Warn(err)
			}
		}
	}
}

func (m *Manager) updateModeWriteCallback(pw *dbusutil.PropertyWrite) *dbus.Error {
	mode := pw.Value.(uint64)
	m.PropsMu.RLock()
	recordMode := m.UpdateMode
	m.PropsMu.RUnlock()
	if recordMode&system.SystemUpdate == system.SystemUpdate && mode&system.SystemUpdate != system.SystemUpdate { // 系统更新由1->0,则同步关闭应用更新
		mode &^= system.AppStoreUpdate
		pw.Value = mode

	} else if mode&system.AppStoreUpdate == system.AppStoreUpdate { // 如果开启应用更新,则强制打开系统更新
		mode |= system.SystemUpdate
		pw.Value = mode
	}

	return dbusutil.ToError(m.config.SetUpdateMode(mode))
}
