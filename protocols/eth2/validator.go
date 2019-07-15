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

//Package eth2 provides functions to assist with Ethereum2.0 related functionality
package eth2

import (
	"fmt"
	"time"
	"regexp"
	"github.com/whiteblock/mustache"
	"github.com/whiteblock/genesis/db"
	"github.com/whiteblock/genesis/ssh"
	"github.com/whiteblock/genesis/util"
	"github.com/whiteblock/genesis/testnet"
	"github.com/whiteblock/genesis/protocols/helpers"
	"github.com/whiteblock/genesis/protocols/ethereum"
)

//DeploySmartContract will deploy the smart validator smart contract
func DeploySmartContract(tn *testnet.TestNet) (string, error) {
	// compile vyper smart contract
	// return contractAddress

	//nodes 0 will be where the contract will be deployed from
	deployNode := tn.Nodes[0]
	deployClient := tn.Clients[deployNode.Server]

	err := helpers.CreateConfigs(tn, "/eth2/smartcontracts/validator_registration.v.py", func(node ssh.Node) ([]byte, error) {
		template, err := helpers.GetBlockchainConfig("eth2", node.GetAbsoluteNumber(), "validator_registration.v.py", tn.LDD)
		if err != nil {
			return nil, util.LogError(err)
		}
		data, err := mustache.Render(string(template), "")
		if err != nil {
			return nil, util.LogError(err)
		}
		return []byte(data), nil
	})
	if err != nil {
		return "", util.LogError(err)
	}

	err = helpers.AllNewNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		_, err := client.DockerExec(node, "npm init -y")
		if err != nil {
			return util.LogError(err)
		}
		_, err = client.DockerExec(node, "npm install web3@1.0.0-beta.31")
		if err != nil {
			return util.LogError(err)
		}
		_, err = client.DockerExec(node, "npm install solc@0.4.25")
		if err != nil {
			return util.LogError(err)
		}
		return nil
	})
	if err != nil {
		return "", util.LogError(err)
	}

	// compile + deploy smart contract
	deployContractOut, err := deployClient.DockerExec(deployNode, fmt.Sprintf("node deploy.js /eth2/smartcontracts/validator_registration.v.py %s", deployNode.IP))
	if err != nil {
		return "", util.LogError(err)
	}
	re := regexp.MustCompile(`(?m)0x[0-9a-fA-F]{40}`)
	addrList := re.FindAllString(deployContractOut, -1)

	return addrList[1], nil
}

//SendDeposit will have every account send a deposit to the deployed smart contract
func SendDeposit(tn *testnet.TestNet, accounts []*ethereum.Account, contractAddr string) error {
	//use every account and send a transaction to the deployed smart contract
	//curl command to send deposit

	err := helpers.AllNodeExecCon(tn, func(client ssh.Client, _ *db.Server, node ssh.Node) error {
		var err error
		for _, account:= range accounts { //give it some extra tries
			_, err = client.KeepTryRun(
				fmt.Sprintf(
					`curl -sS -X POST http://%s:8545 -H "Content-Type: application/json"  -d `+
						`'{ "method": "personal_sendTransaction", "params": ["to:%s, from:%s"], "id": 1, "jsonrpc": "2.0" }'`,
					node.GetIP(), contractAddr, account.PublicKey))
			if err == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
		tn.BuildState.IncrementBuildProgress()
		if err != nil {
			return util.LogError(err)
		}
		return nil
	})
	if err!=nil {
		util.LogError(err)
	}
	return nil
}

/*

func deployContract(fileName, IP string) string {
	fmt.Println("Deploying Smart Contract: " + fileName)
	cwd := os.Getenv("HOME")
	deployCmd := exec.Command("node", "deploy.js", fileName, IP)
	deployCmd.Dir = cwd + "/smart-contracts/"
	output, err := deployCmd.Output()
	if err != nil {
		fmt.Println(err)
	}
	fmt.Printf("%s", output)
	return fmt.Sprintf("%s", output)
}

*/



