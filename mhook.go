package main

import (
	"crypto/md5"
	"fmt"
	"github.com/andrew-d/go-termutil"
	"github.com/cheggaaa/pb"
	"github.com/codegangsta/cli"
	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/s3"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
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
	Bucket      *s3.Bucket
	Project     string
	Branch      string
	Commit      string
	Target      string
	Destination string
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

func targetPathWritable(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err == nil {
		return fi.IsDir(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Head prints the git hash of the latest version
func Head(opts *FetchOptions) string {
	path := fmt.Sprintf("/%s/%s/HEAD", opts.Project, opts.Branch)
	data, err := opts.Bucket.Get(path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// Fetch fetches target from path specified in opts
func Fetch(opts *FetchOptions, target string, showProgress bool) {
	path := fmt.Sprintf("/%s/%s/%s/%s", opts.Project, opts.Branch, opts.Commit, target)

	targetPath := filepath.Dir(opts.Destination)
	writable, err := targetPathWritable(targetPath)
	if !writable || err != nil {
		fmt.Printf("Cannot write to target `%s`. Please check that it exists and is writable.\n", targetPath)
		return
	}

	resp, err := GetResponseETag(opts.Bucket, path, readMD5Sum(opts.Destination))
	if err != nil {
		panic(err)
	}
	if resp == nil {
		return
	}
	defer resp.Body.Close()

	dest, err := os.Create(opts.Destination)
	if err != nil {
		panic(err)
	}
	defer dest.Close()

	bar := pb.New(int(resp.ContentLength)).SetUnits(pb.U_BYTES)
	if showProgress {
		bar.Start()
	}
	writer := io.MultiWriter(dest, bar)

	// write to temporary file
	if _, err = io.Copy(writer, resp.Body); err != nil {
		panic(err)
	}
}

func getBucket(c *cli.Context) (*s3.Bucket, error) {
	// This function checks the keys, the environment vars, the instance metadata, and a cred file
	auth, err := aws.GetAuth(c.String("access-key"), c.String("secret-key"), "", time.Now().Add(time.Minute*5))
	if err != nil {
		return nil, err
	}
	conn := s3.New(auth, aws.Regions[c.String("region")])
	bucket := conn.Bucket(c.String("bucket"))
	return bucket, nil
}

func collectOptions(c *cli.Context) *FetchOptions {

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

	bucket, err := getBucket(c)
	if err != nil {
		panic(err)
	}

	return &FetchOptions{
		Bucket:  bucket,
		Project: c.String("project"),
		Branch:  c.String("branch"),
		Commit:  c.String("commit"),
	}
}

func globalFlags() []cli.Flag {
	return []cli.Flag{
		cli.StringFlag{Name: "bucket, b", Value: "", Usage: "S3 bucket"},
		cli.StringFlag{Name: "project, p", Value: "", Usage: "project name"},
		cli.StringFlag{Name: "branch, r", Value: "master", Usage: "git branch"},
		cli.StringFlag{Name: "access-key", Value: "", Usage: "AWS access key", EnvVar: "AWS_ACCESS_KEY_ID"},
		cli.StringFlag{Name: "secret-key", Value: "", Usage: "AWS access key", EnvVar: "AWS_SECRET_ACCESS_KEY"},
		cli.StringFlag{Name: "region", Value: "us-east-1", Usage: "AWS region"},
	}
}

func fetchFlags() []cli.Flag {
	flags := []cli.Flag{
		cli.StringFlag{Name: "commit, c", Value: "latest", Usage: "git commit (or 'latest')"},
	}
	flags = append(flags, globalFlags()...)
	return flags
}

func main() {
	app := cli.NewApp()
	app.Name = "mhook"
	app.Usage = "[global options] path [dest]"
	app.Flags = fetchFlags()
	app.Commands = []cli.Command{
		{
			Name:  "head",
			Usage: "print latest commit",
			Action: func(c *cli.Context) {
				opts := collectOptions(c)
				fmt.Print(Head(opts))
			},
			Flags: globalFlags(),
		},
	}
	app.Action = func(c *cli.Context) {
		// Check for credentials and well-formedness, then call Fetch

		if len(c.Args()) < 1 {
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		opts := collectOptions(c)

		target := c.Args()[0]

		if len(c.Args()) < 2 {
			// Our destination file will be the same name as our basename
			opts.Destination = path.Base(c.Args()[0])
		} else {
			opts.Destination = c.Args()[1]
		}

		Fetch(opts, target, termutil.Isatty(os.Stdout.Fd()))
	}

	app.Run(os.Args)
}
