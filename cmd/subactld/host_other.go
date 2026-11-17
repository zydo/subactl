//go:build !darwin

package main

import "github.com/zydo/subactl/internal/capacity"

func darwinHost() capacity.Reader { return nil }
