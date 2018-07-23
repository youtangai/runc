// +build linux

package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli"
	"golang.org/x/net/context"
	"log"

	pb "github.com/opencontainers/runc/proto"
	"google.golang.org/grpc"
	"io"
	"net"
)

var migrationServerCommand = cli.Command{
	Name:  "migrationsrv",
	Usage: "restore a container from a recieving previous checkpoint",
	ArgsUsage: `<container-id> <ip> <port>

Where "<container-id>" is the name for the instance of the container to be
restored.`,
	Description: `Restores the saved state of the container instance that was previously saved
using the runc checkpoint command.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "console-socket",
			Value: "",
			Usage: "path to an AF_UNIX socket which will receive a file descriptor referencing the master end of the console's pseudoterminal",
		},
		cli.StringFlag{
			Name:  "image-path",
			Value: "",
			Usage: "path to criu image files for restoring",
		},
		cli.StringFlag{
			Name:  "work-path",
			Value: "",
			Usage: "path for saving work files and logs",
		},
		cli.BoolFlag{
			Name:  "tcp-established",
			Usage: "allow open tcp connections",
		},
		cli.BoolFlag{
			Name:  "ext-unix-sk",
			Usage: "allow external unix sockets",
		},
		cli.BoolFlag{
			Name:  "shell-job",
			Usage: "allow shell jobs",
		},
		cli.BoolFlag{
			Name:  "file-locks",
			Usage: "handle file locks, for safety",
		},
		cli.StringFlag{
			Name:  "manage-cgroups-mode",
			Value: "",
			Usage: "cgroups mode: 'soft' (default), 'full' and 'strict'",
		},
		cli.StringFlag{
			Name:  "bundle, b",
			Value: "",
			Usage: "path to the root of the bundle directory",
		},
		cli.BoolFlag{
			Name:  "detach,d",
			Usage: "detach from the container's process",
		},
		cli.StringFlag{
			Name:  "pid-file",
			Value: "",
			Usage: "specify the file to write the process id to",
		},
		cli.BoolFlag{
			Name:  "no-subreaper",
			Usage: "disable the use of the subreaper used to reap reparented processes",
		},
		cli.BoolFlag{
			Name:  "no-pivot",
			Usage: "do not use pivot root to jail process inside rootfs.  This should be used whenever the rootfs is on top of a ramdisk",
		},
		cli.StringSliceFlag{
			Name:  "empty-ns",
			Usage: "create a namespace, but don't restore its properties",
		},
		cli.BoolFlag{
			Name:  "auto-dedup",
			Usage: "enable auto deduplication of memory images",
		},
		cli.BoolFlag{
			Name:  "lazy-pages",
			Usage: "use userfaultfd to lazily restore memory pages",
		},
		cli.BoolFlag{
			Name:  "track-mem",
			Usage: "allow tracking memory restore proccess added by yota",
		},
	},
	Action: func(context *cli.Context) error {
		if err := checkArgs(context, 3, exactArgs); err != nil {
			return err
		}
		// XXX: Currently this is untested with rootless containers.
		rootless, err := isRootless(context)
		if err != nil {
			return err
		}
		if rootless {
			return fmt.Errorf("runc restore requires root")
		}

		host := context.Args().Get(1)
		port := context.Args().Get(2)
		lis, err := net.Listen("tcp", host+":"+port)
		if err != nil {
			log.Fatalf("failed to listen on host:%s port:%s\n", host, port)
		}
		log.Printf("start listening on host:%s port:%s\n", host, port)
		grpcServer := grpc.NewServer()
		fts := &fileTransferService{
			context:    context,
			folderName: context.String("image-path"),
			fileName:   "",
			mode:       0,
		}
		pb.RegisterFileTransferServiceServer(grpcServer, fts)
		log.Printf("start grpcServer!!\n")
		grpcServer.Serve(lis)
		return nil
	},
}

const (
	DEF_FILE_PERM = 0755
)

type fileTransferService struct {
	context    *cli.Context
	folderName string
	fileName   string
	mode       uint32
}

func (fts *fileTransferService) GetFolderInfo(ctx context.Context, info *pb.FolderInfo) (*pb.Res, error) {
	//fts.folderName = info.Name
	log.Printf("recieve folder name: %s", info.Name)
	if err := os.Mkdir(fts.folderName, DEF_FILE_PERM); err != nil {
		return &pb.Res{Message: "failed to create dir"}, err
	}
	return &pb.Res{Message: "success"}, nil
}

func (fts *fileTransferService) GetFileInfo(ctx context.Context, info *pb.FileInfo) (*pb.Res, error) {
	fts.fileName = info.Name
	fts.mode = info.Mode
	log.Printf("recieve file info{ name:%s, mode:%d}\n", info.Name, info.Mode)
	return &pb.Res{Message: "success!!"}, nil
}

func (fts *fileTransferService) TransferFile(stream pb.FileTransferService_TransferFileServer) error {
	file, err := os.OpenFile(fts.folderName+"/"+fts.fileName, os.O_WRONLY|os.O_CREATE, os.FileMode(fts.mode))
	if err != nil {
		log.Fatalf("cannot open file: %v\n", err)
	}
	defer file.Close()
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			stream.SendAndClose(&pb.Res{Message: "file read done!"})
			return nil
		}
		if err != nil {
			return err
		}
		log.Println(string(req.Data))
		file.Write(req.Data)
	}
}

func (fts *fileTransferService) RestoreContainer(ctx context.Context, info *pb.ContainerInfo) (*pb.Res, error) {
	context := fts.context
	spec, err := setupSpec(context)
	if err != nil {
		return &pb.Res{Message: "failed to load spec"}, err
	}
	options := criuOptions(context)
	status, err := startContainer(context, spec, CT_ACT_RESTORE, options)
	if err != nil {
		return &pb.Res{Message: "failed to restore container"}, err
	}
	// exit with the container's exit status so any external supervisor is
	// notified of the exit with the correct exit status.
	os.Exit(status)
	return &pb.Res{Message: "done restore container"}, nil
}
