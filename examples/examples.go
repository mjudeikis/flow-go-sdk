/*
 * Flow Go SDK
 *
 * Copyright 2019-2020 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package examples

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/onflow/cadence"
	jsoncdc "github.com/onflow/cadence/encoding/json"
	"github.com/onflow/cadence/runtime/sema"
	"io/ioutil"
	"os"
	"time"

	"github.com/onflow/flow-go-sdk"
	"github.com/onflow/flow-go-sdk/client"
	"github.com/onflow/flow-go-sdk/crypto"
	"github.com/onflow/flow-go-sdk/templates"
)

const configPath = "./flow.json"

var (
	conf config
)

type key struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Context struct {
		PrivateKey string `json:"privateKey"`
	}
	SignatureAlgorithm string `json:"signatureAlgorithm"`
	HashAlgorithm      string `json:"hashAlgorithm"`
}

type config struct {
	Accounts struct {
		Service struct {
			Address string `json:"address"`
			Keys    []key
		}
	}
	Contracts map[string]string `json:"contracts"`
}

// ReadFile reads a file from the file system.
func ReadFile(path string) string {
	contents, err := ioutil.ReadFile(path)
	Handle(err)

	return string(contents)
}

func readConfig() config {
	f, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Emulator examples must be run from the flow-go-sdk/examples directory. Please see flow-go-sdk/examples/README.md for more details.")
		} else {
			fmt.Printf("Failed to load config from %s: %s\n", configPath, err.Error())
		}

		os.Exit(1)
	}

	var conf config
	err = json.NewDecoder(f).Decode(&conf)
	Handle(err)

	return conf
}

func init() {
	conf = readConfig()
}

func ServiceAccount(flowClient *client.Client) (flow.Address, *flow.AccountKey, crypto.Signer) {
	sigAlgo := crypto.StringToSignatureAlgorithm(conf.Accounts.Service.Keys[0].SignatureAlgorithm)
	privateKey, err := crypto.DecodePrivateKeyHex(sigAlgo, conf.Accounts.Service.Keys[0].Context.PrivateKey)
	Handle(err)

	addr := flow.HexToAddress(conf.Accounts.Service.Address)
	acc, err := flowClient.GetAccount(context.Background(), addr)
	Handle(err)

	accountKey := acc.Keys[0]
	signer := crypto.NewInMemorySigner(privateKey, accountKey.HashAlgo)
	return addr, accountKey, signer
}

// RandomPrivateKey returns a randomly generated ECDSA P-256 private key.
func RandomPrivateKey() crypto.PrivateKey {
	seed := make([]byte, crypto.MinSeedLength)
	_, err := rand.Read(seed)
	Handle(err)

	privateKey, err := crypto.GeneratePrivateKey(crypto.ECDSA_P256, seed)
	Handle(err)

	return privateKey
}

func RandomAccount(flowClient *client.Client) (flow.Address, *flow.AccountKey, crypto.Signer) {
	privateKey := RandomPrivateKey()

	accountKey := flow.NewAccountKey().
		FromPrivateKey(privateKey).
		SetHashAlgo(crypto.SHA3_256).
		SetWeight(flow.AccountKeyWeightThreshold)

	account := CreateAccount(flowClient, []*flow.AccountKey{accountKey})
	FundAccountInEmulator(flowClient, account.Address, 10.0)
	signer := crypto.NewInMemorySigner(privateKey, accountKey.HashAlgo)
	return account.Address, account.Keys[0], signer
}

func GetReferenceBlockId(flowClient *client.Client) flow.Identifier {
	block, err := flowClient.GetLatestBlock(context.Background(), true)
	Handle(err)

	return block.ID
}

func CreateAccountWithContracts(flowClient *client.Client, publicKeys []*flow.AccountKey, contracts []templates.Contract) *flow.Account {
	serviceAcctAddr, serviceAcctKey, serviceSigner := ServiceAccount(flowClient)

	referenceBlockID := GetReferenceBlockId(flowClient)

	createAccountTx := templates.CreateAccount(publicKeys, contracts, serviceAcctAddr)
	createAccountTx.
		SetProposalKey(serviceAcctAddr, serviceAcctKey.Index, serviceAcctKey.SequenceNumber).
		SetReferenceBlockID(referenceBlockID).
		SetPayer(serviceAcctAddr)

	err := createAccountTx.SignEnvelope(serviceAcctAddr, serviceAcctKey.Index, serviceSigner)
	Handle(err)

	ctx := context.Background()
	err = flowClient.SendTransaction(ctx, *createAccountTx)
	Handle(err)

	result := WaitForSeal(ctx, flowClient, createAccountTx.ID())
	Handle(result.Error)

	for _, event := range result.Events {

		if event.Type != flow.EventAccountCreated {
			continue
		}
		accountCreatedEvent := flow.AccountCreatedEvent(event)

		addr := accountCreatedEvent.Address()
		account, err := flowClient.GetAccount(ctx, addr)
		Handle(err)

		return account
	}
	panic("could not find an AccountCreatedEvent")
}

/**
 * mintTokensToAccountTemplate transaction mints tokens by using the service account (in the emulator)
 * and deposits them to the recipient.
 */
var mintTokensToAccountTemplate = `
import FungibleToken from 0x%s
import FlowToken from 0x%s

transaction(recipient: Address, amount: UFix64) {
	let tokenAdmin: &FlowToken.Administrator
	let tokenReceiver: &{FungibleToken.Receiver}

	prepare(signer: AuthAccount) {
		self.tokenAdmin = signer
			.borrow<&FlowToken.Administrator>(from: /storage/flowTokenAdmin)
			?? panic("Signer is not the token admin")

		self.tokenReceiver = getAccount(recipient)
			.getCapability(/public/flowTokenReceiver)
			.borrow<&{FungibleToken.Receiver}>()
			?? panic("Unable to borrow receiver reference")
	}

	execute {
		let minter <- self.tokenAdmin.createNewMinter(allowedAmount: amount)
		let mintedVault <- minter.mintTokens(amount: amount)

		self.tokenReceiver.deposit(from: <-mintedVault)

		destroy minter
	}
}
`

/**
 * FundAccountInEmulator Mints FLOW to an account. Minting only works in an emulator environment.
 */
func FundAccountInEmulator(flowClient *client.Client, address flow.Address, amount float64) {
	serviceAcctAddr, serviceAcctKey, serviceSigner := ServiceAccount(flowClient)

	referenceBlockID := GetReferenceBlockId(flowClient)

	fungibleTokenAddress := flow.HexToAddress(conf.Contracts["FungibleToken"])
	flowTokenAddress := flow.HexToAddress(conf.Contracts["FlowToken"])

	recipient := cadence.NewAddress(address)
	uintAmount := uint64(amount * sema.Fix64Factor)
	cadenceAmount := cadence.UFix64(uintAmount)

	fundAccountTx :=
		flow.NewTransaction().
			SetScript([]byte(fmt.Sprintf(mintTokensToAccountTemplate, fungibleTokenAddress, flowTokenAddress))).
			AddAuthorizer(serviceAcctAddr).
			AddRawArgument(jsoncdc.MustEncode(recipient)).
			AddRawArgument(jsoncdc.MustEncode(cadenceAmount)).
			SetProposalKey(serviceAcctAddr, serviceAcctKey.Index, serviceAcctKey.SequenceNumber).
			SetReferenceBlockID(referenceBlockID).
			SetPayer(serviceAcctAddr)

	err := fundAccountTx.SignEnvelope(serviceAcctAddr, serviceAcctKey.Index, serviceSigner)
	Handle(err)

	ctx := context.Background()
	err = flowClient.SendTransaction(ctx, *fundAccountTx)
	Handle(err)

	result := WaitForSeal(ctx, flowClient, fundAccountTx.ID())
	Handle(result.Error)
}

func CreateAccount(flowClient *client.Client, publicKeys []*flow.AccountKey) *flow.Account {
	return CreateAccountWithContracts(flowClient, publicKeys, nil)
}

func Handle(err error) {
	if err != nil {
		fmt.Println("err:", err.Error())
		panic(err)
	}
}

func WaitForSeal(ctx context.Context, c *client.Client, id flow.Identifier) *flow.TransactionResult {
	result, err := c.GetTransactionResult(ctx, id)
	Handle(err)

	fmt.Printf("Waiting for transaction %s to be sealed...\n", id)

	for result.Status != flow.TransactionStatusSealed {
		time.Sleep(time.Second)
		fmt.Print(".")
		result, err = c.GetTransactionResult(ctx, id)
		Handle(err)
	}

	fmt.Println()
	fmt.Printf("Transaction %s sealed\n", id)
	return result
}
