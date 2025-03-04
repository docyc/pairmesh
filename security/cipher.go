// Copyright 2021 PairMesh, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package security

import (
	"github.com/flynn/noise"
)

var (
	// CipherSuite represents the cipher suite which is used to handshake between node and relay server.
	CipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

	// HandshakePatternNN represents the handshake pattern which is used to exchange the DH key.
	HandshakePatternNN = noise.HandshakeNN
	HandshakePatternIK = noise.HandshakeIK
)
