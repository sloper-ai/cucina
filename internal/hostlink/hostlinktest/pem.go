// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlinktest

import "encoding/pem"

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
