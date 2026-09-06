// migrate-client-billing converts a stopped CPA usage-stats directory to USD.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
)

func main() {
	dir := flag.String("dir", "", "Offline usage-stats directory (required)")
	dry := flag.Bool("dry-run", false, "Validate and report without rewriting usage files")
	configPath := flag.String("config", "", "Optional stopped CPA configuration to convert to cost-limits")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "--dir is required; stop CPA and back up its data first")
		os.Exit(2)
	}
	configData, changed, err := prepareConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	report, err := redisqueue.MigrateUsageBilling(*dir, *dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if changed && !*dry {
		if err = replaceConfig(*configPath, configData); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err = json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func prepareConfig(path string) ([]byte, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("configuration must be a YAML mapping")
	}
	root := doc.Content[0]
	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "api-keys" {
			continue
		}
		for _, node := range root.Content[i+1].Content {
			if node.Kind != yaml.MappingNode {
				continue
			}
			var entry config.APIKeyEntry
			if err = node.Decode(&entry); err != nil {
				return nil, false, err
			}
			hasCost := false
			for j := 0; j+1 < len(node.Content); j += 2 {
				if node.Content[j].Value == "cost-limits" {
					hasCost = true
				}
			}
			for j := 0; j+1 < len(node.Content); j += 2 {
				if node.Content[j].Value != "token-limits" {
					continue
				}
				changed = true
				if hasCost {
					node.Content = append(node.Content[:j], node.Content[j+2:]...)
					j -= 2
					continue
				}
				node.Content[j].Value = "cost-limits"
				if err = node.Content[j+1].Encode(entry.CostLimits); err != nil {
					return nil, false, err
				}
			}
		}
	}
	if !changed {
		return data, false, nil
	}
	data, err = yaml.Marshal(&doc)
	return data, true, err
}

func replaceConfig(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".billing-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(info.Mode().Perm()); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
