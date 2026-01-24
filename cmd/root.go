// Package cmd provides the CLI commands for gmn.
// Copyright 2025 Tomohiro Owada
// SPDX-License-Identifier: Apache-2.0
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tomohiro-owada/gmn/internal/api"
	"github.com/tomohiro-owada/gmn/internal/auth"
	"github.com/tomohiro-owada/gmn/internal/config"
	"github.com/tomohiro-owada/gmn/internal/input"
	"github.com/tomohiro-owada/gmn/internal/output"
)

var (
	version = "dev"

	prompt       string
	model        string
	outputFormat string
	files        []string
	timeout      time.Duration
	debug        bool
)

var rootCmd = &cobra.Command{
	Use:   "gmn [prompt]",
	Short: "A lightweight, non-interactive Gemini CLI",
	Long: `gmn is a lightweight reimplementation of Google's Gemini CLI
focused on non-interactive use cases. It reuses authentication from
the official Gemini CLI (~/.gemini/).

Examples:
  gmn "Hello, world"
  gmn "Explain Go generics" -m gemini-2.5-pro
  cat file.go | gmn "Review this code"
  gmn "Add error handling" -f main.go`,
	RunE:    run,
	Version: version,
	Args:    cobra.MaximumNArgs(1),
}

func init() {
	rootCmd.Flags().StringVarP(&prompt, "prompt", "p", "", "Prompt to send to Gemini (required)")
	rootCmd.Flags().StringVarP(&model, "model", "m", "gemini-2.5-flash", "Model to use")
	rootCmd.Flags().StringVarP(&outputFormat, "output-format", "o", "text", "Output format: text, json, stream-json")
	rootCmd.Flags().StringArrayVarP(&files, "file", "f", nil, "Files to include in context")
	rootCmd.Flags().DurationVarP(&timeout, "timeout", "t", 5*time.Minute, "API timeout")
	rootCmd.Flags().BoolVar(&debug, "debug", false, "Enable debug output")

}

// Execute runs the root command
func Execute() error {
	return rootCmd.Execute()
}

// SetVersion sets the version string
func SetVersion(v string) {
	version = v
	rootCmd.Version = v
}

func run(cmd *cobra.Command, args []string) error {
	// Handle positional argument as prompt
	if len(args) > 0 {
		prompt = args[0]
	}
	// Setup context with timeout and signal handling
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Create formatter
	formatter, err := output.NewFormatter(outputFormat, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}

	// Load config
	cfg, err := config.Load()
	if err != nil {
		formatter.WriteError(fmt.Errorf("failed to load config: %w", err))
		return err
	}
	_ = cfg // Will be used for MCP

	// Load credentials
	authMgr, err := auth.NewManager()
	if err != nil {
		formatter.WriteError(fmt.Errorf("failed to initialize auth: %w", err))
		return err
	}

	creds, err := authMgr.LoadCredentials()
	if err != nil {
		formatter.WriteError(err)
		return err
	}

	// Refresh if expired
	if creds.IsExpired() {
		if debug {
			fmt.Fprintln(os.Stderr, "Token expired, refreshing...")
		}
		creds, err = authMgr.RefreshToken(creds)
		if err != nil {
			formatter.WriteError(err)
			return err
		}
	}

	// Prepare input
	inputText, err := input.PrepareInput(prompt, files)
	if err != nil {
		formatter.WriteError(err)
		return err
	}

	if inputText == "" {
		err := fmt.Errorf("no input provided")
		formatter.WriteError(err)
		return err
	}

	// Create API client
	httpClient := authMgr.HTTPClient(creds)
	apiClient := api.NewClient(httpClient)

	// Try to load cached project ID first
	cachedState, _ := config.LoadCachedState()
	projectID := cachedState.ProjectID

	// If no cached project ID, fetch from API
	if projectID == "" {
		if debug {
			fmt.Fprintln(os.Stderr, "Loading Code Assist status...")
		}
		loadResp, err := apiClient.LoadCodeAssist(ctx)
		if err != nil {
			formatter.WriteError(fmt.Errorf("failed to load Code Assist: %w", err))
			return err
		}
		projectID = loadResp.CloudAICompanionProject

		// Cache the project ID for next time
		userTier := ""
		if loadResp.CurrentTier != nil {
			userTier = loadResp.CurrentTier.ID
		}
		_ = config.SaveCachedState(&config.CachedState{
			ProjectID: projectID,
			UserTier:  userTier,
		})

		if debug {
			fmt.Fprintf(os.Stderr, "Project ID: %s (cached)\n", projectID)
			if loadResp.CurrentTier != nil {
				fmt.Fprintf(os.Stderr, "Tier: %s\n", loadResp.CurrentTier.ID)
			}
		}
	} else if debug {
		fmt.Fprintf(os.Stderr, "Using cached Project ID: %s\n", projectID)
	}

	// Generate a simple user prompt ID
	userPromptID := fmt.Sprintf("gmn-%d", time.Now().UnixNano())

	// Build request (Code Assist API format)
	req := &api.GenerateRequest{
		Model:        model,
		Project:      projectID,
		UserPromptID: userPromptID,
		Request: api.InnerRequest{
			Contents: []api.Content{{
				Role:  "user",
				Parts: []api.Part{{Text: inputText}},
			}},
			Config: api.GenerationConfig{
				Temperature:     1.0,
				TopP:            0.95,
				MaxOutputTokens: 8192,
			},
		},
	}

	// Execute based on output format
	switch outputFormat {
	case "json":
		return runNonStreaming(ctx, apiClient, req, formatter)
	default:
		return runStreaming(ctx, apiClient, req, formatter)
	}
}

func runNonStreaming(ctx context.Context, client *api.Client, req *api.GenerateRequest, formatter output.Formatter) error {
	resp, err := client.Generate(ctx, req)
	if err != nil {
		formatter.WriteError(err)
		return err
	}
	return formatter.WriteResponse(resp)
}

func runStreaming(ctx context.Context, client *api.Client, req *api.GenerateRequest, formatter output.Formatter) error {
	stream, err := client.GenerateStream(ctx, req)
	if err != nil {
		formatter.WriteError(err)
		return err
	}

	for event := range stream {
		if event.Type == "error" {
			formatter.WriteError(fmt.Errorf(event.Error))
			return fmt.Errorf(event.Error)
		}
		if err := formatter.WriteStreamEvent(&event); err != nil {
			return err
		}
	}

	return nil
}
