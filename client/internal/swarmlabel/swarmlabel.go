// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmlabel names the Docker labels swarmexec reads but does not own.
//
// They are `docker stack deploy`'s, and they are the only thing that makes a
// stack a stack: the engine has no stack object, just services, networks,
// secrets and configs wearing the same namespace label. Writing the string out
// in each package that looks for it meant six copies of a key whose exact
// spelling decides whether a stack is found at all.
package swarmlabel

const (
	// StackNamespace names the stack an object belongs to.
	StackNamespace = "com.docker.stack.namespace"

	// StackImage records the image reference as it was written in the compose
	// file, before the digest was resolved. `docker stack deploy` sets it;
	// swarmexec strips it when it renders a spec back out as compose, so a
	// round trip does not grow a field the operator never wrote.
	StackImage = "com.docker.stack.image"
)
