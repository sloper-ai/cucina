// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig

import (
	"maps"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_runner"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/credentials"
	authpb "github.com/buildbarn/bb-storage/pkg/proto/auth"
	grpcpb "github.com/buildbarn/bb-storage/pkg/proto/configuration/grpc"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

// RenderRunner renders bb_runner's configuration (bb_runner.ApplicationConfiguration,
// pinned bb-remote-execution schema). One bb_runner process per worker serves
// every runner platform: bb_worker points all its RunnerConfigurations at the
// same UNIX socket and adds per-runner environment (QEMU_LD_PREFIX) itself, so
// the native runner and the qemu runners (Linux) or the Xcode and generic runners
// (macOS) share one process (ADR 0410).
func RenderRunner(s *cucinav1.WorkerSettings, m Machine) ([]byte, error) {
	p, err := PlanWorker(s, m)
	if err != nil {
		return nil, err
	}
	return marshal(runnerConfiguration(&m, p))
}

func runnerConfiguration(m *Machine, p *WorkerPlan) *bb_runner.ApplicationConfiguration {
	cfg := &bb_runner.ApplicationConfiguration{
		BuildDirectoryPath: p.BuildDirectoryPath,
		GrpcServers: []*grpcpb.ServerConfiguration{{
			// Only processes that can reach RunDir (root, or the build user on
			// macOS) can connect.
			ListenPaths:          []string{p.RunnerSocket},
			AuthenticationPolicy: &grpcpb.AuthenticationPolicy{Policy: &grpcpb.AuthenticationPolicy_Allow{Allow: &authpb.AuthenticationMetadata{}}},
		}},
		// TMPDIR (TMP/TEMP on Windows) inside the action's build directory: temp
		// files land in the file pool and disappear with the action.
		SetTmpdirEnvironmentVariable: true,
		// chrootIntoInputRoot stays false: actions use the image's toolchains.
	}
	switch m.OS {
	case OSLinux:
		if u := m.BuildUser; u != nil {
			// R-SEC-5: bb_runner (root) runs actions as the unprivileged build user
			// and, whenever it goes idle, kills processes that user left behind.
			cfg.RunCommandsAs = &credentials.UNIXCredentialsConfiguration{
				UserId:             u.UID,
				GroupId:            u.GID,
				AdditionalGroupIds: u.AdditionalGIDs,
			}
			cfg.CleanProcessTable = true
		}
	case OSDarwin:
		// bb_runner runs in the build user's GUI session (R-MAC-4), which also
		// runs unrelated processes, so the process table is not cleaned.
		cfg.AppleXcodeDeveloperDirectories = maps.Clone(m.XcodeDeveloperDirectories)
	}
	return cfg
}
