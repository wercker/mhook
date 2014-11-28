mhook
=====

Simple command-line tool to fetch files from S3 that have been stored using
the `mhook` ultimate freshness layout (MUFL).

Where available it will attempt to use the EC2 metadata to get credentials.

The MUFL layout::

  s3://$bucket/$project/$branch/HEAD        <- contains id of latest commit
  s3://$bucket/$project/$branch/latest/*    <- latest artifacts
  s3://$bucket/$project/$branch/$commit/*   <- artifacts at commit id


Example::

  curl -o mhook https://s3.amazonaws.com/wercker-development/mhook/master/latest/linux_amd64/build
  chmod +x mhook
  ./mhook -b wercker-development -p mhook darwin_amd64/build mhook.darwin_amd64


Usage::

  NAME:
     mhook - [global options] path [dest]

  USAGE:
     mhook [global options] command [command options] [arguments...]

  VERSION:
     0.0.0

  COMMANDS:
     help, h	Shows a list of commands or help for one command

  GLOBAL OPTIONS:
     --bucket, -b 	S3 bucket
     --project, -p 	project name
     --branch 'master'	git branch
     --commit 'latest'	git commit (or 'latest')
     --access-key 		AWS access key [$AWS_ACCESS_KEY_ID]
     --secret-key 		AWS access key [$AWS_SECRET_ACCESS_KEY]
     --region 'us-east-1'	AWS region
     --help, -h		show help
     --version, -v	print the version
