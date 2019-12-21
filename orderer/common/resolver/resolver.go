/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package resolver

import (
	"github.com/hyperledger/fabric/common/flogging"
	//jce "github.com/hyperledger/fabric/orderer/common/johnsonce"
	jce "github.com/khaoNEU/fabric-sigmod/orderer/common/johnsonce"
	"github.com/op/go-logging"
)

const pkgLogID = "orderer/common/resolver"

var logger *logging.Logger

func init() {
	logger = flogging.MustGetLogger(pkgLogID)
}

//resolver接口，包括了Getschedule()方法
type Resolver interface {
	GetSchedule() ([]int32, []bool)
}

//resolver包含了两个graph，使用邻接表进行存储
type resolver struct {
	graph    *[][]int32 // original graph represented as adjacency list
	invgraph *[][]int32 // inverted graph represented as adjacency list
}

func NewResolver(graph *[][]int32, invgraph *[][]int32) Resolver {
	return &resolver{
		graph:    graph,
		invgraph: invgraph,
	}
}

//该方法可以理解为主方法，通过调用这个方法获得最终的串行调度
func (res *resolver) GetSchedule() ([]int32, []bool) {
	// get an instance of dependency resolver
	dagGenerator := jce.NewJohnsonCE(res.graph)

	// run cycle breaker, and retrieve the number of invalidated vertices
	// and the invalid vertices set
	invCount, invSet := dagGenerator.Run()

	nvertices := int32(len(*(res.graph)))

	// track visited vertices
	visited := make([]bool, nvertices)

	// store the schedule
	schedule := make([]int32, 0, nvertices-invCount)

	// track number of processed vertices
	remainingVertices := nvertices - invCount

	// start vertex
	start := int32(0)

	for remainingVertices != 0 {
		addVertex := true
		if visited[start] || invSet[start] {
			start = (start + 1) % nvertices
			continue
		}

		// if there are no incoming edges, start traversal
		// otherwise traverse the inv graph to find the parent
		// which has no incoming edge.
		for _, in := range (*(res.invgraph))[start] {
			if (visited[in] || invSet[in]) == false {
				start = in
				addVertex = false
				break
			}
		}
		if addVertex {
			visited[start] = true
			remainingVertices -= 1
			schedule = append(schedule, start)
			for _, n := range (*(res.graph))[start] {
				if (visited[n] || invSet[n]) == false {
					start = n
					break
				}
			}
		}
	}

	return schedule, invSet
}
