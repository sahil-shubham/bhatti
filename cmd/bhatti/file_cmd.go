package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

var fileCmd = &cobra.Command{
	Use:   "file <read|write|ls> <id|name> <path>",
	Short: "Read, write, and list files in a sandbox",
	Example: `  bhatti file read dev /workspace/app.js
  echo 'hello' | bhatti file write dev /workspace/greeting.txt
  bhatti file ls dev /workspace/`,
}

var fileReadCmd = &cobra.Command{
	Use:               "read <id|name> <path>",
	Short:             "Read a file from a sandbox",
	Args:              exactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		resp, err := apiRequest("GET",
			"/sandboxes/"+id+"/files?path="+url.QueryEscape(args[1]), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return apiErrorFromResponse(resp)
		}
		io.Copy(os.Stdout, resp.Body)
		return nil
	},
}

var fileWriteCmd = &cobra.Command{
	Use:               "write <id|name> <path>",
	Short:             "Write a file to a sandbox (reads from stdin)",
	Args:              exactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		if err := putFile(id, args[1], os.Stdin); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	},
}

// putFile uploads in to guestPath. The API needs a Content-Length, so a
// regular file (`< huge.img`) is streamed with its remaining size — it may be
// far larger than RAM or the guest disk — and anything else (a pipe) is
// buffered first.
func putFile(id, guestPath string, in *os.File) error {
	var body io.Reader = http.NoBody
	var size int64
	if st, err := in.Stat(); err == nil && st.Mode().IsRegular() {
		pos, err := in.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if size = st.Size() - pos; size > 0 {
			// ReadAt, not the shared offset: after an early answer the transport may
			// still be draining the body, and must not move the caller's file.
			body = io.NewSectionReader(in, pos, size)
		}
	} else {
		data, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		if size = int64(len(data)); size > 0 {
			body = bytes.NewReader(data)
		}
	}
	req, err := http.NewRequest("PUT",
		apiURL+"/sandboxes/"+id+"/files?path="+url.QueryEscape(guestPath), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiErrorFromResponse(resp)
	}
	return nil
}

var fileLSCmd = &cobra.Command{
	Use:               "ls <id|name> <path>",
	Short:             "List files in a sandbox directory",
	Args:              exactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var files []struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			IsDir bool   `json:"is_dir"`
			Mode  string `json:"mode"`
		}
		if err := apiJSON("GET",
			"/sandboxes/"+id+"/files?path="+url.QueryEscape(args[1])+"&ls=true",
			nil, &files); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(files)
		} else {
			for _, f := range files {
				dirFlag := "-"
				if f.IsDir {
					dirFlag = "d"
				}
				fmt.Printf("%s%s %8d %s\n", dirFlag, f.Mode, f.Size, f.Name)
			}
		}
		return nil
	},
}

func init() {
	fileCmd.AddCommand(fileReadCmd)
	fileCmd.AddCommand(fileWriteCmd)
	fileCmd.AddCommand(fileLSCmd)
}
