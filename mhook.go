package main

import (
	"crypto/md5"
	"fmt"
	"github.com/cheggaaa/pb"
	"github.com/codegangsta/cli"
	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/s3"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"time"
)

// Simple command-line tool to fetch files from S3 that have been stored using
// the `mhook` ultimate freshness layout (MUFL).
//
// Where available it will attempt to use the EC2 metadata to get credentials.
//
// The MUFL layout:
//
// s3://$bucket/$project/$branch/HEAD		<- contains id of latest commit
// s3://$bucket/$project/$branch/latest/*	<- latest artifacts
// s3://$bucket/$project/$branch/$commit/*	<- artifacts at commit id

type FetchOptions struct {
	Bucket      string
	Project     string
	Branch      string
	Commit      string
	Target      string
	Destination string
	Auth        aws.Auth
	Region      string
}

type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expiration   string
}

func readMD5Sum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	hasher := md5.New()

	if _, err := io.Copy(hasher, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

func GetResponseETag(b *s3.Bucket, path string, etag string) (*http.Response, error) {
	headers := make(http.Header)
	headers.Add("If-None-Match", etag)
	resp, err := b.GetResponseWithHeaders(path, headers)
	if resp != nil {
		return resp, err
	}
	if err.Error() == "304 Not Modified" {
		return nil, nil
	}
	return nil, err
}

func Fetch(opts *FetchOptions) {
	path := fmt.Sprintf("/%s/%s/%s/%s", opts.Project, opts.Branch, opts.Commit, opts.Target)
	conn := s3.New(opts.Auth, aws.Regions[opts.Region])
	bucket := conn.Bucket(opts.Bucket)

	resp, err := GetResponseETag(bucket, path, readMD5Sum(opts.Destination))
	if err != nil {
		panic(err)
	}
	if resp == nil {
		return
	}
	defer resp.Body.Close()

	tmpDst, err := ioutil.TempFile("", opts.Project)
	if err != nil {
		panic(err)
	}
	defer tmpDst.Close()

	bar := pb.New(int(resp.ContentLength)).SetUnits(pb.U_BYTES)
	bar.Start()
	writer := io.MultiWriter(tmpDst, bar)

	// write to temporary file
	if _, err = io.Copy(writer, resp.Body); err != nil {
		panic(err)
	}

	// atomically rename to destiation
	if err = os.Rename(tmpDst.Name(), opts.Destination); err != nil {
		panic(err)
	}
}

func main() {
	app := cli.NewApp()
	app.Name = "mhook"
	app.Usage = "[global options] path [dest]"
	app.Flags = []cli.Flag{
		cli.StringFlag{Name: "bucket, b", Value: "", Usage: "S3 bucket"},
		cli.StringFlag{Name: "project, p", Value: "", Usage: "project name"},
		cli.StringFlag{Name: "branch, r", Value: "master", Usage: "git branch"},
		cli.StringFlag{Name: "commit, c", Value: "latest", Usage: "git commit (or 'latest')"},
		cli.StringFlag{Name: "access-key", Value: "", Usage: "AWS access key", EnvVar: "AWS_ACCESS_KEY_ID"},
		cli.StringFlag{Name: "secret-key", Value: "", Usage: "AWS access key", EnvVar: "AWS_SECRET_ACCESS_KEY"},
		cli.StringFlag{Name: "region", Value: "us-east-1", Usage: "AWS region"},
	}
	app.Action = func(c *cli.Context) {
		// Check for credentials and well-formedness, then call Fetch

		if c.String("bucket") == "" {
			println("Error: bucket cannot be empty.")
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		if c.String("project") == "" {
			println("Error: project cannot be empty.")
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		if len(c.Args()) < 1 {
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		opts := &FetchOptions{
			Target:  c.Args()[0],
			Bucket:  c.String("bucket"),
			Project: c.String("project"),
			Branch:  c.String("branch"),
			Commit:  c.String("commit"),
			Region:  c.String("region"),
		}

		opts.Target = c.Args()[0]

		if len(c.Args()) < 2 {
			// Our destination file will be the same name as our basename
			opts.Destination = path.Base(c.Args()[0])
		} else {
			opts.Destination = c.Args()[1]
		}

		// This function checks the keys, the environment vars, the instance metadata, and a cred file
		auth, err := aws.GetAuth(c.String("access-key"), c.String("secret-key"), "", time.Now().Add(time.Minute*5))
		if err != nil {
			panic(err)
		}
		opts.Auth = auth

		Fetch(opts)
	}

	app.Run(os.Args)
}
