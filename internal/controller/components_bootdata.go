// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"fmt"
	"os"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
)

// EC2 boot data (agent `agent`): the public, non-secret document in user data
// that tells cucina-worker-agent where the EnrollmentService is and which CA
// verifies it (contracts §5.2). The CA bundle is re-read per launch, so a CA
// rotation (two roots) reaches new workers without a restart.
func init() {
	UserDataFunc = func(cfg *config.Controller) func(pool, generation string) ([]byte, error) {
		return func(pool, generation string) ([]byte, error) {
			ca, err := os.ReadFile(cfg.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("boot data: reading the CA bundle: %w", err)
			}
			return bootdata.Encode(bootdata.BootData{
				EnrollEndpoint: cfg.Endpoints.WorkerEnroll,
				ServerName:     cfg.Endpoints.ServerName,
				CAPEM:          string(ca),
				Cluster:        cfg.ClusterID,
				Pool:           pool,
				Generation:     generation,
			})
		}
	}
}
