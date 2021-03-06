// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/seed"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/strutil"
	"github.com/snapcore/snapd/timings"
)

func init() {
	const (
		short = "Generate initramfs mount tuples"
		long  = "Generate mount tuples for the initramfs until nothing more can be done"
	)

	if _, err := parser.AddCommand("initramfs-mounts", short, long, &cmdInitramfsMounts{}); err != nil {
		panic(err)
	}

	snap.SanitizePlugsSlots = func(*snap.Info) {}
}

type cmdInitramfsMounts struct{}

func (c *cmdInitramfsMounts) Execute(args []string) error {
	return generateInitramfsMounts()
}

var (
	// the kernel commandline - can be overridden in tests
	procCmdline = "/proc/cmdline"

	// Stdout - can be overridden in tests
	stdout io.Writer = os.Stdout
)

var (
	runMnt = "/run/mnt"

	osutilIsMounted = osutil.IsMounted
)

// generateMountsMode* is called multiple times from initramfs until it
// no longer generates more mount points and just returns an empty output.
func generateMountsModeInstall(recoverySystem string) error {
	seedDir := filepath.Join(runMnt, "ubuntu-seed")

	// 1. always ensure seed partition is mounted
	isMounted, err := osutilIsMounted(seedDir)
	if err != nil {
		return err
	}
	if !isMounted {
		fmt.Fprintf(stdout, "/dev/disk/by-label/ubuntu-seed %s\n", seedDir)
		return nil
	}

	// 2. (auto) select recovery system for now
	isBaseMounted, err := osutilIsMounted(filepath.Join(runMnt, "base"))
	if err != nil {
		return err
	}
	isKernelMounted, err := osutilIsMounted(filepath.Join(runMnt, "kernel"))
	if err != nil {
		return err
	}
	isSnapdMounted, err := osutilIsMounted(filepath.Join(runMnt, "snapd"))
	if err != nil {
		return err
	}
	if !isBaseMounted || !isKernelMounted || !isSnapdMounted {
		// load the recovery system  and generate mounts for kernel/base
		systemSeed, err := seed.Open(seedDir, recoverySystem)
		if err != nil {
			return err
		}
		// load assertions into a temporary database
		if err := systemSeed.LoadAssertions(nil, nil); err != nil {
			return err
		}
		perf := timings.New(nil)
		// XXX: LoadMeta will verify all the snaps in the
		// seed, that is probably too much. We can expose more
		// dedicated helpers for this later.
		if err := systemSeed.LoadMeta(perf); err != nil {
			return err
		}
		// XXX: do we need more cross checks here?
		for _, essentialSnap := range systemSeed.EssentialSnaps() {
			snapf, err := snap.Open(essentialSnap.Path)
			if err != nil {
				return err
			}
			info, err := snap.ReadInfoFromSnapFile(snapf, essentialSnap.SideInfo)
			if err != nil {
				return err
			}
			switch info.GetType() {
			case snap.TypeBase:
				if !isBaseMounted {
					fmt.Fprintf(stdout, "%s %s\n", essentialSnap.Path, filepath.Join(runMnt, "base"))
				}
			case snap.TypeKernel:
				if !isKernelMounted {
					// XXX: we need to cross-check the kernel path with snapd_recovery_kernel used by grub
					fmt.Fprintf(stdout, "%s %s\n", essentialSnap.Path, filepath.Join(runMnt, "kernel"))
				}
			case snap.TypeSnapd:
				if !isSnapdMounted {
					fmt.Fprintf(stdout, "%s %s\n", essentialSnap.Path, filepath.Join(runMnt, "snapd"))
				}
			}
		}
	}

	// 3. mount "ubuntu-data" on a tmpfs
	isMounted, err = osutilIsMounted(filepath.Join(runMnt, "ubuntu-data"))
	if err != nil {
		return err
	}
	if !isMounted {
		// XXX: is there a better way?
		fmt.Fprintf(stdout, "--type=tmpfs tmpfs /run/mnt/ubuntu-data\n")
		return nil
	}

	// 4. final step: write $(ubuntu_data)/var/lib/snapd/modeenv - this
	//    is the tmpfs we just created above
	modeEnv := &boot.Modeenv{
		Mode:           "install",
		RecoverySystem: recoverySystem,
	}
	if err := modeEnv.Write(filepath.Join(runMnt, "ubuntu-data", "system-data")); err != nil {
		return err
	}

	// 5. done, no output, no error indicates to initramfs we are done
	//    with mounting stuff
	return nil
}

func generateMountsModeRecover(recoverySystem string) error {
	return fmt.Errorf("recover mode mount generation not implemented yet")
}

func generateMountsModeRun() error {
	seedDir := filepath.Join(runMnt, "ubuntu-seed")
	bootDir := filepath.Join(runMnt, "ubuntu-boot")
	dataDir := filepath.Join(runMnt, "ubuntu-data")

	// 1.1 always ensure basic partitions are mounted
	for _, d := range []string{seedDir, bootDir} {
		isMounted, err := osutilIsMounted(d)
		if err != nil {
			return err
		}
		if !isMounted {
			fmt.Fprintf(stdout, "/dev/disk/by-label/%s %s\n", filepath.Base(d), d)
		}
	}

	// XXX possibly will need to unseal key, and unlock LUKS here before proceeding to mount data

	// 1.2 mount Data, and exit, as it needs to be mounted for us to do step 2
	isDataMounted, err := osutilIsMounted(dataDir)
	if err != nil {
		return err
	}
	if !isDataMounted {
		fmt.Fprintf(stdout, "/dev/disk/by-label/%s %s\n", filepath.Base(dataDir), dataDir)
		return nil
	}
	// 2.1 read modeenv
	modeEnv, err := boot.ReadModeenv(filepath.Join(dataDir, "system-data"))
	if err != nil {
		return err
	}
	// 2.2 mount base & kernel
	isBaseMounted, err := osutilIsMounted(filepath.Join(runMnt, "base"))
	if err != nil {
		return err
	}
	if !isBaseMounted {
		base := filepath.Join(dataDir, "system-data", dirs.SnapBlobDir, modeEnv.Base)
		fmt.Fprintf(stdout, "%s %s\n", base, filepath.Join(runMnt, "base"))
	}
	isKernelMounted, err := osutilIsMounted(filepath.Join(runMnt, "kernel"))
	if err != nil {
		return err
	}
	if !isKernelMounted {
		// XXX: do we need to cross-check the booted/running kernel vs the snap?
		kernel := filepath.Join(dataDir, "system-data", dirs.SnapBlobDir, modeEnv.Kernel)
		fmt.Fprintf(stdout, "%s %s\n", kernel, filepath.Join(runMnt, "kernel"))
	}
	// 3.1 There is no step 3 =)
	return nil
}

var validModes = []string{"install", "recover", "run"}

func whichModeAndRecoverSystem(cmdline []byte) (mode string, sysLabel string, err error) {
	scanner := bufio.NewScanner(bytes.NewBuffer(cmdline))
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "snapd_recovery_mode=") {
			mode = strings.SplitN(scanner.Text(), "=", 2)[1]
			if mode == "" {
				mode = "install"
			}
			if !strutil.ListContains(validModes, mode) {
				return "", "", fmt.Errorf("cannot use unknown mode %q", mode)
			}
			if mode == "run" {
				return "run", "", nil
			}
		}
		if strings.HasPrefix(scanner.Text(), "snapd_recovery_system=") {
			sysLabel = strings.SplitN(scanner.Text(), "=", 2)[1]
		}
		if mode != "" && sysLabel != "" {
			return mode, sysLabel, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("cannot detect mode nor recovery system to use")
}

func generateInitramfsMounts() error {
	cmdline, err := ioutil.ReadFile(procCmdline)
	if err != nil {
		return err
	}
	mode, recoverySystem, err := whichModeAndRecoverSystem(cmdline)
	if err != nil {
		return err
	}
	switch mode {
	case "recover":
		return generateMountsModeRecover(recoverySystem)
	case "install":
		return generateMountsModeInstall(recoverySystem)
	case "run":
		return generateMountsModeRun()
	}
	// this should never be reached
	return fmt.Errorf("internal error: mode in generateInitramfsMounts not handled")
}
