/*
	Copyright 2019 whiteblock Inc.
	This file is a part of the genesis.

	Genesis is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	Genesis is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

//Package parity handles parity specific functionality
package parity

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/whiteblock/genesis/db"
	"github.com/whiteblock/genesis/protocols/ethclassic"
	"github.com/whiteblock/genesis/protocols/ethereum"
	"github.com/whiteblock/genesis/protocols/helpers"
	"github.com/whiteblock/genesis/protocols/registrar"
	"github.com/whiteblock/genesis/ssh"
	"github.com/whiteblock/genesis/testnet"
	"github.com/whiteblock/genesis/util"
	"strings"
	"sync"
	"time"
)

var conf = util.GetConfig()

const (
	blockchain   = "parity"
	password     = "password"
	passwordFile = "/parity/passwd"
)

func init() {
	registrar.RegisterBuild(blockchain, build)
	registrar.RegisterAddNodes(blockchain, add)
	registrar.RegisterServices(blockchain, GetServices)
	registrar.RegisterDefaults(blockchain, helpers.DefaultGetDefaultsFn(blockchain))
	registrar.RegisterParams(blockchain, helpers.DefaultGetParamsFn(blockchain))

	registrar.RegisterBlockchainSideCars(blockchain, func(tn *testnet.TestNet) []string {
		pconf, err := newConf(tn.LDD.Extras)
		if err != nil {
			util.LogError(err)
			return nil
		}
		if pconf.Consensus == "ethash" {
			return []string{"geth"}
		}
		return nil
	})
}

// build builds out a fresh new ethereum test network using parity
func build(tn *testnet.TestNet) error {
	mux := sync.Mutex{}
	pconf, err := newConf(tn.LDD.Params)
	if err != nil {
		return util.LogError(err)
	}
	log.WithFields(log.Fields{"config": *pconf}).Trace("parsed the parity config")

	tn.BuildState.SetBuildSteps(9 + (6 * tn.LDD.Nodes))
	//Make the data directories
	err = helpers.MkdirAllNodes(tn, "/parity")
	if err != nil {
		return util.LogError(err)
	}
	tn.BuildState.IncrementBuildProgress()

	err = ethereum.CreatePasswordFile(tn, password, passwordFile)
	if err != nil {
		return util.LogError(err)
	}
	tn.BuildState.IncrementBuildProgress()

	/**Create the wallets**/
	wallets := make([]string, tn.LDD.Nodes)
	rawWallets := make([]string, tn.LDD.Nodes)
	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		res, err := client.DockerExec(node, "parity --base-path=/parity/ --password=/parity/passwd account new")
		if err != nil {
			return util.LogError(err)
		}

		if len(res) == 0 {
			return fmt.Errorf("account new returned an empty response")
		}

		mux.Lock()
		wallets[node.GetAbsoluteNumber()] = res[:len(res)-1]
		mux.Unlock()

		res, err = client.DockerExec(node, "bash -c 'cat /parity/keys/ethereum/*'")
		if err != nil {
			return util.LogError(err)
		}
		tn.BuildState.IncrementBuildProgress()

		mux.Lock()
		rawWallets[node.GetAbsoluteNumber()] = strings.Replace(res, "\"", "\\\"", -1)
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	/***********************************************************SPLIT************************************************************/
	switch pconf.Consensus {
	case "ethash":
		err = setupPOW(tn, pconf, wallets)
	case "poa":
		err = setupPOA(tn, pconf, wallets)
	default:
		return util.LogError(fmt.Errorf("Unknown consensus %s", pconf.Consensus))
	}
	if err != nil {
		return util.LogError(err)
	}

	/***********************************************************SPLIT************************************************************/

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, rawWallet := range rawWallets {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\">/parity/account%d'", rawWallet, i))
			if err != nil {
				return util.LogError(err)
			}

			_, err = client.DockerExec(node,
				fmt.Sprintf("parity --base-path=/parity/ --chain /parity/spec.json --password=/parity/passwd account import /parity/account%d", i))
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		defer tn.BuildState.IncrementBuildProgress()
		return client.DockerRunMainDaemon(node,
			fmt.Sprintf(`parity --author=%s -c /parity/config.toml --chain=/parity/spec.json`, wallets[node.GetAbsoluteNumber()]))
	})
	if err != nil {
		return util.LogError(err)
	}
	//Start peering via curl
	time.Sleep(time.Duration(5 * time.Second))
	//Get the enode addresses
	enodes := make([]string, tn.LDD.Nodes)
	err = helpers.AllNodeExecCon(tn, func(client ssh.Client, server *db.Server, node ssh.Node) error {
		enode := ""
		for len(enode) == 0 {
			res, err := client.KeepTryRun(
				fmt.Sprintf(
					`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json" `+
						` -d '{ "method": "parity_enode", "params": [], "id": 1, "jsonrpc": "2.0" }'`,
					node.GetIP()))

			if err != nil {
				return util.LogError(err)
			}
			var result map[string]interface{}

			err = json.Unmarshal([]byte(res), &result)
			if err != nil {
				return util.LogError(err)
			}
			log.WithFields(log.Fields{"result": result}).Trace("fetched enode addr from parity_enode")

			err = util.GetJSONString(result, "result", &enode)
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		mux.Lock()
		enodes[node.GetAbsoluteNumber()] = enode
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	storeParameters(tn, pconf, wallets, enodes)
	tn.BuildState.IncrementBuildProgress()
	return peerAllNodes(tn, enodes)
}

/***************************************************************************************************************************/

// Add adds an ETC node to the network
// TODO
func add(tn *testnet.TestNet) error {
	//etc attachment
	mux := sync.Mutex{}

	tn.BuildState.IncrementBuildProgress()

	tn.BuildState.SetBuildStage("Pulling the genesis block")

	var etcGenesisFile ethclassic.EtcConf
	tn.BuildState.GetP("etcconf", &etcGenesisFile)

	var genesisAlloc map[string]map[string]string
	tn.BuildState.GetP("alloc", &genesisAlloc)

	parityConf, err := newConf(tn.LDD.Params)
	tn.BuildState.SetBuildSteps(1 + 2*len(tn.NewlyBuiltNodes)) //TODO
	if err != nil {
		return util.LogError(err)
	}

	parityConf.Name = etcGenesisFile.Name
	parityConf.DataDir = etcGenesisFile.Identity
	parityConf.NetworkID = etcGenesisFile.NetworkID
	parityConf.ChainID = etcGenesisFile.NetworkID
	parityConf.MinimumDifficulty = etcGenesisFile.Difficulty
	parityConf.Difficulty = etcGenesisFile.Difficulty
	parityConf.Mixhash = etcGenesisFile.Mixhash
	parityConf.Nonce = etcGenesisFile.Nonce
	parityConf.Timestamp = etcGenesisFile.Timestamp
	parityConf.ExtraData = etcGenesisFile.ExtraData
	parityConf.GasLimit = etcGenesisFile.GasLimit

	helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		_, err := client.DockerExec(node, fmt.Sprintf("mkdir -p /parity"))
		return err
	})
	if err != nil {
		return util.LogError(err)
	}

	/**Create the Password file and copy it over**/
	err = ethereum.CreatePasswordFile(tn, password, passwordFile)
	if err != nil {
		return util.LogError(err)
	}

	genWallets := []string{}
	wallets := []string{}
	rawWallets := []string{}
	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		res, err := client.DockerExec(node, "parity --base-path=/parity/ --password=/parity/passwd account new")
		if err != nil {
			return util.LogError(err)
		}

		if len(res) == 0 {
			return fmt.Errorf("account new returned an empty response")
		}

		mux.Lock()
		wallets = append(wallets, res[:len(res)-1])
		mux.Unlock()

		res, err = client.DockerExec(node, "bash -c 'cat /parity/keys/ethereum/*'")
		if err != nil {
			return util.LogError(err)
		}
		tn.BuildState.IncrementBuildProgress()

		mux.Lock()
		rawWallets = append(rawWallets, strings.Replace(res, "\"", "\\\"", -1))
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i := 0; i < node.GetAbsoluteNumber(); i++ {
			var nodeKeyStores string
			tn.BuildState.GetP(fmt.Sprintf("node%dKey", i), &nodeKeyStores)
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\" >> /parity/account%d'", nodeKeyStores, i+1))
			if err != nil {
				return err
			}
			rawWallets = append(rawWallets, nodeKeyStores)
		}
		return err
	})
	if err != nil {
		return util.LogError(err)
	}

	for i := range genesisAlloc {
		wallets = append(wallets, "0x"+i)
		genWallets = append(genWallets, "0x"+i)
	}

	// ***********************************************************************************************************

	switch etcGenesisFile.Consensus {
	case "ethash":
		err = setupNewPOW(tn, parityConf, wallets, genWallets)
	case "poa":
		err = setupNewPOA(tn, parityConf, wallets, genWallets)
	default:
		return util.LogError(fmt.Errorf("Unknown consensus %s", parityConf.Consensus))
	}
	if err != nil {
		return util.LogError(err)
	}

	// ***********************************************************************************************************

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, rawWallet := range rawWallets {
			_, err := client.DockerExec(node, fmt.Sprintf("bash -c 'echo \"%s\">/parity/account%d'", rawWallet, i))
			if err != nil {
				return util.LogError(err)
			}

			_, err = client.DockerExec(node,
				fmt.Sprintf("parity --base-path=/parity/ --chain /parity/spec.json --password=/parity/passwd account import /parity/account%d", i))
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		defer tn.BuildState.IncrementBuildProgress()
		return client.DockerRunMainDaemon(node,
			fmt.Sprintf(`parity --author=%s -c /parity/config.toml --chain=/parity/spec.json`, wallets[node.GetAbsoluteNumber()%tn.LDD.Nodes]))
	})
	if err != nil {
		return util.LogError(err)
	}

	var snodes []string
	tn.BuildState.GetP("staticNodes", &snodes)
	log.WithFields(log.Fields{"enodes": snodes}).Debug("Fetched the enodes from the previous build")
	fmt.Println(fmt.Sprintf("enode address : %+v", snodes))
	if err != nil {
		return util.LogError(err)
	}

	//Start peering via curl
	time.Sleep(time.Duration(5 * time.Second))
	//Get the enode addresses
	enodes := make([]string, tn.LDD.Nodes)
	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, server *db.Server, node ssh.Node) error {
		enode := ""
		for len(enode) == 0 {
			res, err := client.KeepTryRun(
				fmt.Sprintf(
					`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json" `+
						` -d '{ "method": "parity_enode", "params": [], "id": 1, "jsonrpc": "2.0" }'`,
					node.GetIP()))

			if err != nil {
				return util.LogError(err)
			}
			var result map[string]interface{}

			err = json.Unmarshal([]byte(res), &result)
			if err != nil {
				return util.LogError(err)
			}
			log.WithFields(log.Fields{"result": result}).Trace("fetched enode addr from parity_enode")

			err = util.GetJSONString(result, "result", &enode)
			if err != nil {
				return util.LogError(err)
			}
		}
		tn.BuildState.IncrementBuildProgress()
		mux.Lock()
		enodes[node.GetAbsoluteNumber()%tn.LDD.Nodes] = enode
		mux.Unlock()
		return nil
	})
	if err != nil {
		return util.LogError(err)
	}
	storeParameters(tn, parityConf, wallets, enodes)

	tn.BuildState.IncrementBuildProgress()
	tn.BuildState.SetBuildStage("Bootstrapping network")

	return peerAllNodes(tn, snodes)
}

func peerAllNodes(tn *testnet.TestNet, enodes []string) error {
	return helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		for i, enode := range enodes {
			if i == node.GetAbsoluteNumber() {
				continue
			}
			_, err := client.Run(
				fmt.Sprintf(
					`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json"  -d `+
						`'{ "method": "parity_addReservedPeer", "params": ["%s"], "id": 1, "jsonrpc": "2.0" }'`,
					node.GetIP(), enode))
			tn.BuildState.IncrementBuildProgress()
			if err != nil {
				return util.LogError(err)
			}
		}
		return nil
	})
}

func storeParameters(tn *testnet.TestNet, pconf *parityConf, wallets []string, enodes []string) {
	accounts, err := ethereum.GenerateAccounts(tn.LDD.Nodes)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Warn("couldn't create geth accounts")
	}

	tn.BuildState.Set("networkID", pconf.NetworkID)
	tn.BuildState.SetExt("networkID", pconf.NetworkID)
	tn.BuildState.SetExt("port", ethereum.RPCPort)
	ethereum.ExposeAccounts(tn, accounts)

	switch pconf.Consensus {
	case "ethash":
		tn.BuildState.Set("mine", true)
	case "poa":
		tn.BuildState.Set("mine", false)
	}

	tn.BuildState.Set("peers", enodes)

	tn.BuildState.Set("gethConf", map[string]interface{}{
		"networkID":   pconf.NetworkID,
		"initBalance": pconf.InitBalance,
		"difficulty":  fmt.Sprintf("0x%x", pconf.Difficulty),
		"gasLimit":    fmt.Sprintf("0x%x", pconf.GasLimit),
		"extraData":   pconf.ExtraData,
		"consensus":   pconf.Consensus,
		"consensusParams": map[string]interface{}{
			"difficulty": pconf.Difficulty,
		}})

	tn.BuildState.Set("wallets", wallets)
}

func setupPOA(tn *testnet.TestNet, pconf *parityConf, wallets []string) error {
	//Create the chain spec files
	spec, err := buildPoaSpec(pconf, tn.LDD, wallets)
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.CopyBytesToAllNodes(tn, spec, "/parity/spec.json")
	if err != nil {
		return util.LogError(err)
	}

	//handle configuration file
	return helpers.CreateConfigs(tn, "/parity/config.toml",
		func(node ssh.Node) ([]byte, error) {
			configToml, err := buildPoaConfig(pconf, tn.LDD, wallets, "/parity/passwd", node.GetAbsoluteNumber())
			if err != nil {
				return nil, util.LogError(err)
			}
			return []byte(configToml), nil
		})
}

func setupPOW(tn *testnet.TestNet, pconf *parityConf, wallets []string) error {
	tn.BuildState.IncrementBuildProgress()

	//Create the chain spec files
	spec, err := buildSpec(pconf, tn.LDD, wallets)
	if err != nil {
		return util.LogError(err)
	}
	//create config file
	err = helpers.CreateConfigs(tn, "/parity/config.toml", func(node ssh.Node) ([]byte, error) {
		configToml, err := buildConfig(pconf, tn.LDD, wallets, "/parity/passwd", node.GetAbsoluteNumber())
		if err != nil {
			return nil, util.LogError(err)
		}
		return []byte(configToml), nil
	})
	if err != nil {
		return util.LogError(err)
	}
	//Copy over the config file, spec file, and the accounts
	return helpers.CopyBytesToAllNodes(tn, spec, "/parity/spec.json")
}

func setupNewPOA(tn *testnet.TestNet, pconf *parityConf, wallets, genWallets []string) error {
	//Create the chain spec files
	spec, err := buildPoaSpec(pconf, tn.LDD, genWallets)
	if err != nil {
		return util.LogError(err)
	}

	err = helpers.CopyBytesToAllNewNodes(tn, spec, "/parity/spec.json")
	if err != nil {
		return util.LogError(err)
	}

	//handle configuration file
	return helpers.CreateConfigsNewNodes(tn, "/parity/config.toml",
		func(node ssh.Node) ([]byte, error) {
			configToml, err := buildPoaConfig(pconf, tn.LDD, wallets, "/parity/passwd", node.GetAbsoluteNumber())
			if err != nil {
				return nil, util.LogError(err)
			}
			return []byte(configToml), nil
		})
}

func setupNewPOW(tn *testnet.TestNet, pconf *parityConf, wallets, genWallets []string) error {
	tn.BuildState.IncrementBuildProgress()

	//Create the chain spec files
	spec, err := buildSpec(pconf, tn.LDD, genWallets)
	if err != nil {
		return util.LogError(err)
	}
	//create config file
	err = helpers.CreateConfigsNewNodes(tn, "/parity/config.toml", func(node ssh.Node) ([]byte, error) {
		configToml, err := buildConfig(pconf, tn.LDD, wallets, "/parity/passwd", node.GetAbsoluteNumber())
		if err != nil {
			return nil, util.LogError(err)
		}
		return []byte(configToml), nil
	})
	if err != nil {
		return util.LogError(err)
	}
	//Copy over the config file, spec file, and the accounts
	return helpers.CopyBytesToAllNewNodes(tn, spec, "/parity/spec.json")
}
