// SPDX-License-Identifier: FSL-1.1-ALv2

package hostd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// InternetSharingPlist holds the vmnet shared-network DHCP server settings.
const InternetSharingPlist = "/Library/Preferences/SystemConfiguration/com.apple.InternetSharing.default.plist"

// DefaultsVMNet sets the vmnet DHCP lease with `defaults` (root only, R-MAC-5:
// a short lease recycles the 192.168.64.0/24 addresses of re-cloned VMs).
type DefaultsVMNet struct {
	Exec ports.Exec
}

// LeaseArgs is the `defaults write` invocation (merges into the bootpd dictionary).
func LeaseArgs(seconds int) []string {
	return []string{"write", InternetSharingPlist, "bootpd", "-dict-add", "DHCPLeaseTimeSecs", "-int", strconv.Itoa(seconds)}
}

// EnsureLease implements VMNet.
func (d DefaultsVMNet) EnsureLease(ctx context.Context, seconds int) error {
	res, err := d.Exec.Run(ctx, ports.Command{Path: "/usr/bin/defaults", Args: LeaseArgs(seconds)})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("defaults write: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
