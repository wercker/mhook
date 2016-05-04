package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

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
		if err != nil {
			return err
		}
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

// ToLatest returns a copy of `m` with the Commit set to "latest"
func (m *Mhook) ToLatest() *Mhook {
	return &Mhook{
		S3:      m.S3,
		Bucket:  m.Bucket,
		Project: m.Project,
		Branch:  m.Branch,
		Commit:  "latest",
	}
}

// WriteHead writes HEAD key in S3
func (m *Mhook) WriteHead() error {
	_, err := m.S3.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(m.Bucket),
		Key:    m.HeadKey(),
		Body:   bytes.NewReader([]byte(m.Commit)),
	})
	return err
}

// Wait waits until timeout for the key to exist
func (m *Mhook) Wait(target string) error {
	return m.S3.WaitUntilObjectExists(&s3.HeadObjectInput{
		Bucket: aws.String(m.Bucket),
		Key:    m.Key(target),
	})

}

func (m *Mhook) Download(target string, destination string) error {
	manager := s3manager.NewDownloaderWithClient(m.S3)
	prefix := (*m.Key(target))[1:]
	d := downloader{
		Downloader:   manager,
		bucket:       m.Bucket,
		dir:          destination,
		showProgress: m.ShowProgress,
		prefix:       prefix,
	}
	params := &s3.ListObjectsInput{
		Bucket: &m.Bucket,
		Prefix: &prefix,
	}
	if err := m.S3.ListObjectsPages(params, d.eachPage); err != nil {
		return err
	}
	if d.err != nil {
		return d.err
	}
	return nil
}

type downloader struct {
	*s3manager.Downloader
	bucket, dir, prefix string
	showProgress        bool
	err                 error
}

func (d *downloader) eachPage(page *s3.ListObjectsOutput, more bool) bool {
	for _, obj := range page.Contents {
		if err := d.downloadToFile(*obj.Key, *obj.Size); err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				fmt.Println(awsErr.Code(), awsErr.Message(), awsErr.OrigErr())
				if reqErr, ok := err.(awserr.RequestFailure); ok {
					fmt.Println(reqErr.StatusCode(), reqErr.RequestID())
				}
			} else {
				fmt.Println(err.Error())
			}
			d.err = err
			return false
		}
	}
	return true
}

func (d *downloader) downloadToFile(key string, size int64) error {
	// Create the directories in the path
	file := filepath.Join(d.dir, key[len(d.prefix):])
	targetPath := filepath.Dir(file)

	if err := os.MkdirAll(targetPath, 0775); err != nil {
		return err
	}

	temp, err := ioutil.TempFile(targetPath, "mhook-")
	if err != nil {
		return err
	}
	defer temp.Close()
	defer os.Remove(temp.Name())

	bar := pb.New64(size).SetUnits(pb.U_BYTES)
	if d.showProgress {
		bar.Start()
	}
	etag := readMD5Sum(file)
	writer := &progressWriter{temp, bar}

	// Download the file using the AWS SDK
	params := &s3.GetObjectInput{
		Bucket:      &d.bucket,
		Key:         &key,
		IfNoneMatch: &etag,
	}
	if _, err := d.Download(writer, params); err != nil {
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			if reqErr.StatusCode() == 304 {
				bar.Set64(bar.Total)
				bar.FinishPrint(fmt.Sprintf("Using local copy for %s", file))
				return nil
			}
			return reqErr
		}
		return err
	}
	bar.FinishPrint(fmt.Sprintf("Downloaded %s", file))

	if err := os.Rename(temp.Name(), file); err != nil {
		return err
	}
	return nil

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
	config := aws.NewConfig().WithRegion(c.String("region")).WithMaxRetries(10)
	if c.Bool("debug") {
		config = config.WithLogLevel(aws.LogDebugWithRequestRetries)
	}
	sess := session.New(config)
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
		cli.BoolFlag{Name: "debug", Usage: "enable debug logging"},
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
			if err := mhook.Download(target, destination); err != nil {
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
			if c.Bool("latest") {
				if err := mhook.WriteHead(); err != nil {
					panic(err)
				}
				if err := mhook.ToLatest().Upload(source, prefix); err != nil {
					panic(err)
				}
			}
		},
		Flags: append(
			targetFlags(),
			cli.BoolFlag{Name: "latest", Usage: "Tag this upload as latest, " +
				"copying it to the `latest` folder and creating a HEAD file."},
		),
	}
)

var (
	// GitCommit is the git commit hash associated with this build.
	GitCommit = "dev"

	// Compiled is the unix timestamp when this binary got compiled.
	Compiled = ""
)

// CompiledAt converts the Unix time Compiled to a time.Time using UTC timezone.
func compiledAt() (*time.Time, error) {
	i, err := strconv.ParseInt(Compiled, 10, 64)
	if err != nil {
		return nil, err
	}
	t := time.Unix(i, 0).UTC()

	return &t, nil
}

func getVersion() string {
	compiledWhen, err := compiledAt()
	if err != nil {
		return GitCommit
	}
	return fmt.Sprintf("%s (Compiled at: %s)", GitCommit, compiledWhen.Format(time.RFC3339))
}

func main() {
	app := cli.NewApp()
	app.Name = "mhook"
	app.Usage = "Manage the MUFL"
	// Set downloadCommand as default for backwards compatibility
	app.Version = getVersion()
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
