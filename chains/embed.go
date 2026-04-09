// Package chains embeds the canonical Liquid chain genesis files.
package chains

import _ "embed"

//go:embed evm.json
var EVMGenesis []byte

//go:embed dex.json
var DEXGenesis []byte

//go:embed fhe.json
var FHEGenesis []byte
