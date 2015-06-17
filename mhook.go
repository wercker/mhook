package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/andrew-d/go-termutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cheggaaa/pb"
	"github.com/codegangsta/cli"
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

type Fetcher struct {
	S3          *s3.S3
	Bucket      string
	Project     string
	Branch      string
	Commit      string
	Destination string
}

func (f *Fetcher) HeadKey() *string {
	return aws.String(fmt.Sprintf("/%s/%s/HEAD", f.Project, f.Branch))
}

func (f *Fetcher) Key(target string) *string {
	return aws.String(fmt.Sprintf("/%s/%s/%s/%s", f.Project, f.Branch, f.Commit, target))
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
func Head(opts *Fetcher) string {
	resp, err := opts.S3.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(opts.Bucket),
		Key:    opts.HeadKey(),
	})
	if err != nil {
		panic(err)
	}

	// Pretty-print the response data.
	etag, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return string(etag)
}

func targetSize(opts *Fetcher, target string) *int64 {
	resp, err := opts.S3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(opts.Bucket),
		Key:    opts.Key(target),
	})
	if err != nil {
		panic(err)
	}

	return resp.ContentLength
}

type ProgressWriter struct {
	w  io.WriterAt
	pb *pb.ProgressBar
}

func (pw *ProgressWriter) WriteAt(p []byte, off int64) (int, error) {
	pw.pb.Add(len(p))
	return pw.w.WriteAt(p, off)
}

// Fetch fetches target from path specified in opts
func Fetch(opts *Fetcher, target string, showProgress bool) error {
	targetPath := filepath.Dir(opts.Destination)
	writable, err := targetPathWritable(targetPath)
	if !writable || err != nil {
		fmt.Printf("Cannot write to target `%s`. Please check that it exists and is writable.\n", targetPath)
		return err
	}

	temp, err := ioutil.TempFile(targetPath, fmt.Sprintf(".%s-", opts.Project))
	if err != nil {
		return err
	}
	defer temp.Close()

	bar := pb.New64(*targetSize(opts, target)).SetUnits(pb.U_BYTES)
	if showProgress {
		bar.Start()
	}
	etag := readMD5Sum(opts.Destination)
	writer := &ProgressWriter{temp, bar}

	downloader := s3manager.NewDownloader(&s3manager.DownloadOptions{
		S3: opts.S3,
	})
	_, err = downloader.Download(writer, &s3.GetObjectInput{
		Bucket:      aws.String(opts.Bucket),
		Key:         opts.Key(target),
		IfNoneMatch: aws.String(etag),
	})
	if err != nil {
		os.Remove(temp.Name())
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			if reqErr.StatusCode() == 304 {
				bar.Set64(bar.Total)
				bar.FinishPrint("Using local copy.")
				return nil
			}
			return reqErr
		}
		return err
	}

	return os.Rename(temp.Name(), opts.Destination)
}

func collectOptions(c *cli.Context) *Fetcher {

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

	svc := s3.New(&aws.Config{Region: c.String("region")})
	return &Fetcher{
		S3:      svc,
		Bucket:  c.String("bucket"),
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
			// Our destination file will be the same name as our target basename
			opts.Destination = path.Base(target)
		} else {
			opts.Destination = c.Args()[1]
		}

		if err := Fetch(opts, target, termutil.Isatty(os.Stdout.Fd())); err != nil {
			panic(err)
		}
	}
	app.Run(os.Args)
}
