/*
Copyright the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resourcepolicies

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/vmware-tanzu/velero/test"
	. "github.com/vmware-tanzu/velero/test/e2e/test"
	"github.com/vmware-tanzu/velero/test/util/common"
	. "github.com/vmware-tanzu/velero/test/util/k8s"
	. "github.com/vmware-tanzu/velero/test/util/velero"
)

const (
	excludeImportantDB = "important.db"
	excludeCacheFile   = "cache/session.tmp"
	excludeLogFile     = "logs/app.log"
	excludeKeepTmp     = "keep.tmp"
	excludeOtherTmp    = "other.tmp"
)

type ExcludeFilesCase struct {
	TestCase
	cmName           string
	yamlConfig       string
	snapshotMoveData bool
	expectBackupFail bool
	presentFiles     []string
	absentFiles      []string
	filesToWrite     []string
}

var ExcludeFilesFSBackupTest = TestFunc(&ExcludeFilesCase{
	yamlConfig: `version: v1
volumePolicies:
- conditions:
    storageClass:
    - e2e-storage-class
  action:
    type: fs-backup
    parameters:
      exclude:
      - "/cache/*"
      - "*.log"
`,
	filesToWrite: []string{excludeImportantDB, excludeCacheFile, excludeLogFile},
	presentFiles: []string{excludeImportantDB},
	absentFiles:  []string{excludeCacheFile, excludeLogFile},
})

var ExcludeFilesCSIDataMoverTest = TestFunc(&ExcludeFilesCase{
	snapshotMoveData: true,
	yamlConfig: `version: v1
volumePolicies:
- conditions:
    storageClass:
    - e2e-storage-class
  action:
    type: snapshot
    parameters:
      dataMover: velero-fs
      exclude:
      - "/cache/*"
      - "*.log"
`,
	filesToWrite: []string{excludeImportantDB, excludeCacheFile, excludeLogFile},
	presentFiles: []string{excludeImportantDB},
	absentFiles:  []string{excludeCacheFile, excludeLogFile},
})

var ExcludeFilesBlockRejectTest = TestFunc(&ExcludeFilesCase{
	expectBackupFail: true,
	yamlConfig: `version: v1
volumePolicies:
- conditions:
    storageClass:
    - e2e-storage-class
  action:
    type: snapshot
    parameters:
      dataMover: velero-block
      exclude:
      - "*.tmp"
`,
})

var ExcludeFilesAdditiveTest = TestFunc(&ExcludeFilesCase{
	yamlConfig: `version: v1
volumePolicies:
- conditions:
    storageClass:
    - e2e-storage-class
  action:
    type: fs-backup
    parameters:
      exclude:
      - "*.tmp"
- conditions:
    storageClass:
    - e2e-storage-class
  action:
    type: fs-backup
    parameters:
      exclude:
      - "*.log"
      - "!keep.tmp"
`,
	filesToWrite: []string{excludeImportantDB, excludeKeepTmp, excludeOtherTmp, excludeLogFile},
	presentFiles: []string{excludeImportantDB, excludeKeepTmp},
	absentFiles:  []string{excludeOtherTmp, excludeLogFile},
})

func (e *ExcludeFilesCase) Init() error {
	e.TestCase.Init()

	kind := "fs-backup"
	if e.snapshotMoveData {
		kind = "csi-datamover"
	}
	if e.expectBackupFail {
		kind = "block-reject"
	}
	if len(e.presentFiles) > 1 {
		kind = "additive"
	}

	e.CaseBaseName = "exclude-files-" + kind + "-" + e.UUIDgen
	e.BackupName = "backup-" + e.CaseBaseName
	e.RestoreName = "restore-" + e.CaseBaseName
	e.cmName = "cm-" + e.CaseBaseName
	e.NamespacesTotal = 1
	e.NSIncluded = &[]string{e.CaseBaseName}

	e.VeleroCfg.UseNodeAgent = true
	e.VeleroCfg.UseVolumeSnapshots = e.snapshotMoveData

	e.BackupArgs = []string{
		"create", "--namespace", e.VeleroCfg.VeleroNamespace, "backup", e.BackupName,
		"--resource-policies-configmap", e.cmName,
		"--include-namespaces", e.CaseBaseName,
		"--wait",
	}
	if e.snapshotMoveData {
		e.BackupArgs = append(e.BackupArgs,
			"--snapshot-volumes=true",
			"--default-volumes-to-fs-backup=false",
			"--snapshot-move-data=true",
		)
	} else {
		e.BackupArgs = append(e.BackupArgs,
			"--default-volumes-to-fs-backup",
			"--snapshot-volumes=false",
		)
	}

	if e.expectBackupFail {
		e.RestoreArgs = nil
	} else {
		e.RestoreArgs = []string{
			"create", "--namespace", e.VeleroCfg.VeleroNamespace, "restore", e.RestoreName,
			"--from-backup", e.BackupName, "--wait",
		}
	}

	desc := "Exclude files from volume backup via Volume Policy"
	if e.expectBackupFail {
		desc = "Volume Policy exclude on velero-block is rejected"
	}
	e.TestMsg = &TestMSG{
		Desc:      desc,
		FailedMSG: "Failed exclude-files Volume Policy test " + kind,
		Text:      fmt.Sprintf("Should honor exclude patterns for backup %s", e.BackupName),
	}
	return nil
}

func (e *ExcludeFilesCase) CreateResources() error {
	By(fmt.Sprintf("Create configmap %s in namespace %s", e.cmName, e.VeleroCfg.VeleroNamespace), func() {
		Expect(CreateConfigMapFromYAMLData(e.Client.ClientGo, e.yamlConfig, e.cmName, e.VeleroCfg.VeleroNamespace)).To(Succeed())
	})
	By(fmt.Sprintf("Waiting for configmap %s ready", e.cmName), func() {
		Expect(WaitForConfigMapComplete(e.Client.ClientGo, e.VeleroCfg.VeleroNamespace, e.cmName)).To(Succeed())
	})

	if e.expectBackupFail {
		By(fmt.Sprintf("Create namespace %s", e.CaseBaseName), func() {
			Expect(CreateNamespace(e.Ctx, e.Client, e.CaseBaseName)).To(Succeed())
		})
		return nil
	}

	namespace := e.CaseBaseName
	nsLabels := map[string]string{}
	if e.VeleroCfg.WorkerOS == common.WorkerOSWindows {
		nsLabels = map[string]string{
			"pod-security.kubernetes.io/enforce":         "privileged",
			"pod-security.kubernetes.io/enforce-version": "latest",
		}
	}
	By(fmt.Sprintf("Create namespace %s", namespace), func() {
		Expect(CreateNamespaceWithLabel(e.Ctx, e.Client, namespace, nsLabels)).To(Succeed())
	})

	volName := "vol-data"
	volList := PrepareVolumeList([]string{volName})
	By("Creating PVC", func() {
		pvcBuilder := NewPVC(namespace, "pvc-0").WithStorageClass(StorageClassName)
		Expect(CreatePvc(e.Client, pvcBuilder)).To(Succeed())
	})
	By("Creating deployment", func() {
		deployment := NewDeployment(
			e.CaseBaseName,
			namespace,
			1,
			map[string]string{"exclude-files": "exclude-files"},
			e.VeleroCfg.ImageRegistryProxy,
			e.VeleroCfg.WorkerOS,
		).WithVolume(volList).Result()
		deployment, err := CreateDeployment(e.Client.ClientGo, namespace, deployment)
		Expect(err).NotTo(HaveOccurred())
		Expect(WaitForReadyDeployment(e.Client.ClientGo, namespace, deployment.Name)).To(Succeed())
	})
	By("Writing files into the volume", func() {
		Expect(e.writeFiles(namespace, volName)).To(Succeed())
	})
	return nil
}

func (e *ExcludeFilesCase) Backup() error {
	if !e.expectBackupFail {
		return e.TestCase.Backup()
	}

	veleroCfg := e.GetTestCase().VeleroCfg
	By("Start backup expected to fail validation ......", func() {
		Expect(VeleroBackupExec(e.Ctx, veleroCfg.VeleroCLI, veleroCfg.VeleroNamespace, e.BackupName, e.BackupArgs)).NotTo(Succeed())
	})
	return nil
}

func (e *ExcludeFilesCase) Verify() error {
	if e.expectBackupFail {
		return nil
	}

	ns := e.CaseBaseName
	By(fmt.Sprintf("Verify pod data in namespace %s", ns), func() {
		Expect(WaitForReadyDeployment(e.Client.ClientGo, ns, e.CaseBaseName)).To(Succeed())
		podList, err := ListPods(e.Ctx, e.Client, ns)
		Expect(err).NotTo(HaveOccurred())
		volName := "vol-data"
		for _, pod := range podList.Items {
			for _, want := range e.presentFiles {
				exist, err := FileExistInPV(e.Ctx, ns, pod.Name, "container-busybox", volName, want, e.VeleroCfg.WorkerOS)
				Expect(err).NotTo(HaveOccurred(), "failed checking present file %s", want)
				Expect(exist).To(BeTrue(), "expected file %s to be present after restore", want)
			}
			for _, skip := range e.absentFiles {
				exist, err := FileExistInPV(e.Ctx, ns, pod.Name, "container-busybox", volName, skip, e.VeleroCfg.WorkerOS)
				Expect(err).NotTo(HaveOccurred(), "failed checking absent file %s", skip)
				Expect(exist).To(BeFalse(), "expected file %s to be absent after restore", skip)
			}
		}
	})
	return nil
}

func (e *ExcludeFilesCase) Clean() error {
	if CurrentSpecReport().Failed() && e.VeleroCfg.FailFast {
		fmt.Println("Test case failed and fail fast is enabled. Skip resource clean up.")
		return nil
	}
	_ = DeleteConfigMap(e.Client.ClientGo, e.VeleroCfg.VeleroNamespace, e.cmName)
	return e.GetTestCase().Clean()
}

func (e *ExcludeFilesCase) writeFiles(namespace, volName string) error {
	podList, err := ListPods(e.Ctx, e.Client, namespace)
	if err != nil {
		return err
	}
	for _, pod := range podList.Items {
		dirs := map[string]struct{}{}
		for _, f := range e.filesToWrite {
			if i := strings.LastIndex(f, "/"); i >= 0 {
				dirs[f[:i]] = struct{}{}
			}
		}
		for dir := range dirs {
			if err := mkdirInPod(namespace, pod.Name, "container-busybox", volName, dir, e.VeleroCfg.WorkerOS); err != nil {
				return err
			}
		}
		for _, f := range e.filesToWrite {
			if err := CreateFileToPod(
				namespace,
				pod.Name,
				"container-busybox",
				volName,
				f,
				"",
				e.VeleroCfg.WorkerOS,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func mkdirInPod(namespace, podName, containerName, volume, dir, workerOS string) error {
	path := fmt.Sprintf("/%s/%s", volume, dir)
	shell, param, cmd := "/bin/sh", "-c", fmt.Sprintf("mkdir -p %s", path)
	if workerOS == common.WorkerOSWindows {
		path = fmt.Sprintf("C:\\%s\\%s", volume, strings.ReplaceAll(dir, "/", "\\"))
		shell, param, cmd = "cmd", "/c", fmt.Sprintf("mkdir %s", path)
	}
	arg := []string{"exec", "-n", namespace, "-c", containerName, podName, "--", shell, param, cmd}
	return exec.CommandContext(context.Background(), "kubectl", arg...).Run()
}
