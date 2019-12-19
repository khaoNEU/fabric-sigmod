/*
Copyright IBM Corp. 2017 All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package main is the entrypoint for the orderer binary
// and calls only into the server.Main() function.  No other
// function should be included in this package.
package main

import (
	"fmt"
	"github.com/hyperledger/fabric/orderer/common/server"
)

func main() {
	server.Main()
	fmt.Println("This is cross-chain concurrency control fabric based on sigmod 2019")
}
