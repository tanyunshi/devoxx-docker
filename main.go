package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/rumpl/devoxx-docker/ipc"
	"github.com/rumpl/devoxx-docker/remote"
)

const (
	IMAGE_ROOT    = "/fs/alpine/rootfs" // Placeholder for the image root directory
	CGROUP_ROOT   = IMAGE_ROOT + "/sys/fs/cgroup"
	CGROUP_PATH   = CGROUP_ROOT + "/devoxx-docker"
	MEMORY_MAX    = "104857600"    // 100MB memory limit
	CPU_MAX       = "50000 100000" // 50ms per 100ms period
	PEER_NAME     = "veth1"
	HOST_IP       = "10.0.0.1"
	HOST_IP_RANGE = HOST_IP + "/24"
	CONTAINER_IP  = "10.0.0.2/24"
)

func main() {

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "child":
			if len(os.Args) < 3 {
				log.Fatal("Error: No socket file descriptor provided for child\n")
			}
			socketFD, err := strconv.Atoi(os.Args[2])
			if err != nil {
				log.Fatal(err)
			}

			socket := os.NewFile(uintptr(socketFD), "child-socket")
			if socket == nil {
				log.Fatal("Failed to create socket file from FD")
			}
			if err := child(socket); err != nil {
				log.Fatalf("Error in child process: %v\n", err)
			}
		case "pull":
			if len(os.Args) < 3 {
				log.Fatal("Error: No image specified for pull\n")
			}
			if err := pull(os.Args[2]); err != nil {
				log.Fatalf("Error pulling image: %v\n", err)
			}
		case "run":
			if len(os.Args) < 4 {
				log.Fatal("Error: No command specified for run\n")
			}
			if err := parent(); err != nil {
				log.Fatalf("Error in run process: %v\n", err)
			}
		default:
			if err := parent(); err != nil {
				log.Fatalf("Error in parent process: %v\n", err)
			}
		}
	} else {
		if err := parent(); err != nil {
			log.Fatalf("Error in parent process: %v\n", err)
		}
	}

}

func getVolumeSpec() (string, string) {
	// Get the volume specification from the command line arguments

	if len(os.Args) > 4 && os.Args[4] == "-v" {
		volumeSpec := os.Args[5]
		volumeParts := strings.Split(volumeSpec, ":")

		if len(volumeParts) != 2 {
			return "", fmt.Sprintf("invalid volume specification: %s, expected format source:target", volumeSpec)
		}
		source := volumeParts[0]
		target := volumeParts[1]

		return source, target
	}
	return "", ""
}

func child(socket *os.File) error {
	// volumeDestination := fmt.Sprintf("/fs/%s/rootfs/volume", "alpine")
	// if err := syscall.Mount("/workspaces/devoxx-docker/volume", volumeDestination, "", syscall.MS_PRIVATE|syscall.MS_BIND, ""); err != nil {
	// 	return fmt.Errorf("mount volume %w", err)
	// }

	// Mount the volume to the container path
	// sudo ./bin/devoxx-docker run alpine /bin/sh -v /workspace/volume:/fs/alpine
	defer socket.Close()

	fmt.Printf("Child args are: %v\n", os.Args[2:])

	volumeSrc, volumeDest := getVolumeSpec()
	if volumeSrc != "" && volumeDest != "" {
		if err := mountvolume(volumeSrc, volumeDest); err != nil {
			return fmt.Errorf("error mounting volume: %w", err)
		}

		// Register the unmount function to be called on exit
		defer unmountvolume(volumeDest)
	}

	// Set the container hostname
	if err := syscall.Sethostname([]byte("container")); err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("error getting hostname: %w", err)
	}
	fmt.Printf("Child hostname is: %s\n", hostname)

	setupContainer(os.Args[3])

	// Wait for parent to complete network setup
	if err := ipc.WaitForReady(socket); err != nil {
		return fmt.Errorf("wait for network setup: %w", err)
	}

	setupContainerNetworking()

	// Execute the command passed to the child process
	cmd := exec.Command(os.Args[4])
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func parent() error {

	parentSocket, childSocket, err := ipc.CreateSockerPair()
	if err != nil {
		return fmt.Errorf("error creating socket pair: %w", err)
	}
	defer parentSocket.Close()

	childFd := childSocket.Fd()
	socketFd := strconv.Itoa(int(childFd))

	if err := setupCgroups(); err != nil {
		return fmt.Errorf("setup cgroups %w", err)
	}

	volumeSrc, volumeDest := getVolumeSpec()
	if volumeSrc != "" && volumeDest != "" {
		destFullPath := filepath.Join(IMAGE_ROOT, volumeDest)
		if err := setupvolume(destFullPath); err != nil {
			return fmt.Errorf("setup volume %w", err)
		}
	}

	// Call the same command in a new process
	cmd := exec.Command("/proc/self/exe", append([]string{"child", socketFd}, os.Args[2:]...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{childSocket}

	// Isolate the process from the other processes in the system
	// In linux this is done using namespaces
	// UTS namespace: hostname
	// PID namespace, the processes inside the pid namespace can only see the processes inside the pid namespace
	// Network namespacemake
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %w", err)
	}

	childSocket.Close()

	if err := setupVeth(cmd.Process.Pid); err != nil {
		return fmt.Errorf("setup container networking %w", err)
	}
	defer cleanupVeth()

	if err := addProcessToCgroup(cmd.Process.Pid); err != nil {
		return fmt.Errorf("add process to cgroup %w", err)
	}

	// Setup done, tell child to continue
	if err := ipc.SendReday(parentSocket); err != nil {
		return fmt.Errorf("send ready %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait %w", err)
	}

	fmt.Printf("Container exited with exit code %d\n", cmd.ProcessState.ExitCode())
	return nil
}

func pull(image string) error {
	// Placeholder for pulling an image
	// sudo mkdir /fs/alpine && docker export $(docker create alpine) | tar -C /fs/alpine -xvf -
	fmt.Printf("Pulling image: %s\n", image)
	puller := remote.NewImagePuller(image)
	if err := puller.Pull(); err != nil {
		return fmt.Errorf("error pulling image: %w", err)
	}
	fmt.Printf("Image %s pulled successfully\n", image)
	return nil
}

func setupContainer(image string) error {
	// Placeholder for setting up the container
	// This would typically involve creating a new filesystem namespace, etc.
	fmt.Println("Setting up container...")
	// Print the current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting current working directory: %w", err)
	}
	fmt.Printf("Current working directory: %s\n", cwd)

	// Change root to "/fs/<image>/rootfs"
	rootPath := "/fs/" + image + "/rootfs"
	fmt.Printf("Changing root to %s\n", rootPath)
	if err := syscall.Chroot(rootPath); err != nil {
		return fmt.Errorf("error changing root: %w", err)
	}
	fmt.Printf("Changed root to %s\n", rootPath)

	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir failed: %w", err)
	}

	cwd, err = os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting current working directory: %w", err)
	}
	fmt.Printf("New working directory: %s\n", cwd)

	return nil
}

func setupCgroups() error {
	/*
		Cgroups allows you to group processes and then limit, prioritize, and or monitor
		their usage of resources such as CPU, memory, disk I/O, etc.
		Cmd to run a memory-intensive workload
		# dd if=/dev/zero of=/dev/null bs=1M count=200
	*/
	// Create a cgroup for the child process
	if err := os.MkdirAll(CGROUP_PATH, 0755); err != nil {
		return fmt.Errorf("error creating cgroup directory: %w", err)
	}
	// Set memory limit
	memLimitFile := filepath.Join(CGROUP_PATH, "memory.max")
	if err := os.WriteFile(memLimitFile, []byte(MEMORY_MAX), 0644); err != nil {
		return fmt.Errorf("error writing memory limit: %w", err)
	}
	fmt.Printf("%s: Set memory limit to %s\n", memLimitFile, MEMORY_MAX)
	// Set CPU limit
	cpuLimitFile := filepath.Join(CGROUP_PATH, "cpu.max")
	if err := os.WriteFile(cpuLimitFile, []byte(CPU_MAX), 0644); err != nil {
		return fmt.Errorf("error writing CPU limit: %w", err)
	}
	fmt.Printf("%s: Set CPU limit to %s\n", cpuLimitFile, CPU_MAX)

	fmt.Println("Cgroup setup complete")
	return nil
}

func addProcessToCgroup(childPid int) error {
	// Add the child process to the cgroup
	// Must be down in the parent process after starting the child but beore waiting for it
	cgroupFile := filepath.Join(CGROUP_PATH, "cgroup.procs")
	if err := os.WriteFile(cgroupFile, []byte(fmt.Sprintf("%d", childPid)), 0644); err != nil {
		return fmt.Errorf("error adding process to cgroup: %w", err)
	}
	fmt.Printf("Added PID %d to cgroup: %s\n", childPid, cgroupFile)
	return nil
}

func setupvolume(dest string) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("error creating volume directory: %w", err)
	}
	fmt.Printf("Volume setup complete %s", dest)
	return nil
}

func mountvolume(source string, target string) error {
	// Mount the volume to the container path
	flag := syscall.MS_BIND | syscall.MS_REC | syscall.MS_PRIVATE
	targetFullPath := filepath.Join(IMAGE_ROOT, target)
	fmt.Printf("Mounting %s to %s\n", source, targetFullPath)
	if err := syscall.Mount(source, targetFullPath, "", uintptr(flag), ""); err != nil {
		return fmt.Errorf("error mounting volume: %w", err)
	}
	fmt.Printf("Mounted %s to %s\n", target, source)

	return nil
}

func unmountvolume(target string) error {
	// Unmount the volume from the container path
	if err := syscall.Unmount(target, 0); err != nil {
		if err == syscall.EBUSY {
			fmt.Print("Volume is busy, trying to unmount with forced unmount...\n")
			if err := syscall.Unmount(target, syscall.MNT_FORCE); err != nil {
				return fmt.Errorf("error unmounting volume: %w", err)
			}
		} else {
			return fmt.Errorf("error unmounting volume: %w", err)
		}
	}

	// Clean up the mount point directory
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("error removing target directory: %w", err)
	}

	fmt.Printf("Unmounted %s\n", target)
	return nil
}

func setupVeth(pid int) error {
	// Setup a veth pair for the container
	// This would typically involve creating a virtual Ethernet interface and setting up network namespaces
	// Placeholder for network setup
	// The container interface should be in the 10.0.0.0/24 subnet
	// Common IP assigements:
	// - host interface (veth0): 10.0.0.1
	// - container interface (veth1): 10.0.0.2
	// Required iptables rules should enable NAT for the container subnet

	cmdCreateVethPair := []string{"ip", "link", "add", "veth0", "type", "veth", "peer", "name", PEER_NAME}
	if err := exec.Command(cmdCreateVethPair[0], cmdCreateVethPair[1:]...).Run(); err != nil {
		return fmt.Errorf("error setting up veth: %w", err)
	}

	cmdMoveToNetNs := []string{"ip", "link", "set", PEER_NAME, "netns", fmt.Sprintf("%d", pid)}
	if err := exec.Command(cmdMoveToNetNs[0], cmdMoveToNetNs[1:]...).Run(); err != nil {
		return fmt.Errorf("error moving veth to netns: %w", err)
	}

	cmdAssignIp := []string{"ip", "addr", "add", HOST_IP_RANGE, "dev", "veth0"}
	if err := exec.Command(cmdAssignIp[0], cmdAssignIp[1:]...).Run(); err != nil {
		return fmt.Errorf("error assigning IP to veth: %w", err)
	}

	if err := exec.Command("ip", "link", "set", "veth0", "up").Run(); err != nil {
		return fmt.Errorf("set host veth up %w", err)
	}

	cmdSetupNatRule := []string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", CONTAINER_IP, "-j", "MASQUERADE"}
	if err := exec.Command(cmdSetupNatRule[0], cmdSetupNatRule[1:]...).Run(); err != nil {
		return fmt.Errorf("error setting up NAT rule: %w", err)
	}

	fmt.Printf("Setting up veth for PID %d\n", pid)
	return nil
}

func cleanupVeth() error {
	// Clean up the veth pair
	// This would typically involve deleting the virtual Ethernet interface and cleaning up network namespaces
	// Placeholder for network cleanup
	cmdDeleteVeth := []string{"ip", "link", "del", PEER_NAME}
	if err := exec.Command(cmdDeleteVeth[0], cmdDeleteVeth[1:]...).Run(); err != nil {
		return fmt.Errorf("error deleting veth: %w", err)
	}

	cmdDeleteNatRule := []string{"ip", "-t", "nat", "-D", "POSTROUTING", "-s", CONTAINER_IP, "-j", "MASQUERADE"}
	if err := exec.Command(cmdDeleteNatRule[0], cmdDeleteNatRule[1:]...).Run(); err != nil {
		return fmt.Errorf("error deleting NAT rule: %w", err)
	}

	fmt.Printf("Cleaned up veth\n")
	return nil
}

func setupContainerNetworking() error {
	cmdAssignIp := []string{"ip", "addr", "add", CONTAINER_IP, "dev", PEER_NAME}
	if err := exec.Command(cmdAssignIp[0], cmdAssignIp[1:]...).Run(); err != nil {
		return fmt.Errorf("error assigning IP to container: %w", err)
	}

	cmdIpLinkSetUp := []string{"ip", "link", "set", PEER_NAME, "up"}
	if err := exec.Command(cmdIpLinkSetUp[0], cmdIpLinkSetUp[1:]...).Run(); err != nil {
		return fmt.Errorf("error bringing up the interface: %w", err)
	}

	cmdLookup := []string{"ip", "link", "set", "lo", "up"}
	if err := exec.Command(cmdLookup[0], cmdLookup[1:]...).Run(); err != nil {
		return fmt.Errorf("error bringing up the loopback interface: %w", err)
	}

	cmdAddDefaultRoute := []string{"ip", "route", "add", "default", "via", HOST_IP}
	if err := exec.Command(cmdAddDefaultRoute[0], cmdAddDefaultRoute[1:]...).Run(); err != nil {
		return fmt.Errorf("error adding default route: %w", err)
	}

	fmt.Printf("Container networking setup complete\n")
	return nil
}
