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
	"github.com/aws/aws-sdk-go/aws/session"
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

// Mhook represents the MUFL structure
type Mhook struct {
	S3           *s3.S3
	Bucket       string
	Project      string
	Branch       string
	Commit       string
	Destination  string
	ShowProgress bool
}

// HeadKey gets the key for the HEAD file
func (m *Mhook) HeadKey() *string {
	return aws.String(fmt.Sprintf("/%s/%s/HEAD", m.Project, m.Branch))
}

// Key formats the key for target
func (m *Mhook) Key(target string) *string {
	return aws.String(fmt.Sprintf("/%s/%s/%s/%s", m.Project, m.Branch, m.Commit, target))
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
func Head(m *Mhook) string {
	resp, err := m.S3.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(m.Bucket),
		Key:    m.HeadKey(),
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

func (m *Mhook) targetSize(target string) *int64 {
	resp, err := m.S3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(m.Bucket),
		Key:    m.Key(target),
	})
	if err != nil {
		panic(err)
	}

	return resp.ContentLength
}

type progressWriter struct {
	w  io.WriterAt
	pb *pb.ProgressBar
}

func (pw *progressWriter) WriteAt(p []byte, off int64) (int, error) {
	pw.pb.Add(len(p))
	return pw.w.WriteAt(p, off)
}

// Upload source to s3 in the MUFL format
func (m *Mhook) Upload(source string, prefix string) error {
	uploader := s3manager.NewUploaderWithClient(m.S3)
	walk := func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		bar := pb.New64(info.Size()).SetUnits(pb.U_BYTES)
		if m.ShowProgress {
			bar.Start()
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		reader := io.TeeReader(file, bar)
		uploadInput := &s3manager.UploadInput{
			Bucket: aws.String(m.Bucket),
			Key:    m.Key(prefix + filepath.Base(path)),
			Body:   reader,
		}
		fmt.Println(*uploadInput.Key)
		_, err = uploader.Upload(uploadInput)
		return err
	}
	return filepath.Walk(filepath.Clean(source), walk)
}

// Fetch fetches target from path specified in opts
func (m *Mhook) Fetch(target string, destination string) error {
	targetPath := filepath.Dir(destination)
	writable, err := targetPathWritable(targetPath)
	if !writable || err != nil {
		fmt.Printf("Cannot write to target `%s`. Please check that it exists and is writable.\n", targetPath)
		return err
	}

	temp, err := ioutil.TempFile(targetPath, fmt.Sprintf(".%s-", m.Project))
	if err != nil {
		return err
	}
	defer temp.Close()

	bar := pb.New64(*m.targetSize(target)).SetUnits(pb.U_BYTES)
	if m.ShowProgress {
		bar.Start()
	}
	etag := readMD5Sum(destination)
	writer := &progressWriter{temp, bar}

	downloader := s3manager.NewDownloaderWithClient(m.S3)
	_, err = downloader.Download(writer, &s3.GetObjectInput{
		Bucket:      aws.String(m.Bucket),
		Key:         m.Key(target),
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

	return os.Rename(temp.Name(), destination)
}

// Wait waits until timeout for the key to exist
func (m *Mhook) Wait(target string) error {
	return m.S3.WaitUntilObjectExists(&s3.HeadObjectInput{
		Bucket: aws.String(m.Bucket),
		Key:    m.Key(target),
	})

}

func collectOptions(c *cli.Context) *Mhook {

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
	region := c.String("region")
	sess := session.New(&aws.Config{Region: &region})
	svc := s3.New(sess)
	return &Mhook{
		S3:           svc,
		Bucket:       c.String("bucket"),
		Project:      c.String("project"),
		Branch:       c.String("branch"),
		Commit:       c.String("commit"),
		ShowProgress: termutil.Isatty(os.Stdout.Fd()),
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

func targetFlags() []cli.Flag {
	flags := []cli.Flag{
		cli.StringFlag{Name: "commit, c", Value: "latest", Usage: "git commit (or 'latest')"},
	}
	flags = append(flags, globalFlags()...)
	return flags
}

var (
	headCommand = cli.Command{
		Name:  "head",
		Usage: "Print latest commit.",
		Action: func(c *cli.Context) {
			opts := collectOptions(c)
			fmt.Print(Head(opts))
		},
		Flags: globalFlags(),
	}
	waitCommand = cli.Command{
		Name:  "wait",
		Usage: "Wait until key exists.",
		Action: func(c *cli.Context) {
			if !c.Args().Present() {
				cli.ShowAppHelp(c)
				os.Exit(1)
			}
			mhook := collectOptions(c)
			target := c.Args().First()
			if err := mhook.Wait(target); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		},
		Flags: targetFlags(),
	}
	downloadCommand = cli.Command{
		Name:      "download",
		Usage:     "Download mhook artifact. If no destination is supplied, use the base path of the target.",
		ArgsUsage: "<target> [destination]",
		Action: func(c *cli.Context) {
			// Check for credentials and well-formedness, then call Fetch

			if !c.Args().Present() {
				cli.ShowAppHelp(c)
				os.Exit(1)
			}

			mhook := collectOptions(c)
			var destination string
			target := c.Args().First()

			destination = c.Args().Get(1)
			if destination == "" {
				// Our destination file will be the same name as our target basename
				destination = path.Base(target)
			}

			if c.Bool("wait") {
				if err := mhook.Wait(target); err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
			}

			fmt.Printf("Downloading from %s\n", *mhook.Key(target))
			if err := mhook.Fetch(target, destination); err != nil {
				panic(err)
			}
		},
		Flags: append(
			targetFlags(),
			cli.BoolFlag{Name: "wait", Usage: "wait for key to exist before proceding."},
		),
	}
	uploadCommand = cli.Command{
		Name:      "upload",
		Usage:     "Upload mhook artifact.",
		ArgsUsage: "<source> [upload prefix]",
		Action: func(c *cli.Context) {
			if !c.Args().Present() {
				cli.ShowAppHelp(c)
				os.Exit(1)
			}
			mhook := collectOptions(c)
			source := c.Args().First()
			prefix := c.Args().Get(1)
			// if target is directory, upload it recursively
			if err := mhook.Upload(source, prefix); err != nil {
				panic(err)
			}
		},
		Flags: targetFlags(),
	}
)

func main() {
	app := cli.NewApp()
	app.Name = "mhook"
	app.Usage = "Manage the MUFL"
	// Set downloadCommand as default for backwards compatibility
	app.Flags = downloadCommand.Flags
	app.Commands = []cli.Command{
		headCommand,
		waitCommand,
		downloadCommand,
		uploadCommand,
	}
	app.Action = downloadCommand.Action
	app.Run(os.Args)
}
