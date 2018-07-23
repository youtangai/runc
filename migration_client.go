// +build linux

package main

import (
	"fmt"

	"github.com/opencontainers/runc/libcontainer"
	"github.com/urfave/cli"
	pb "github.com/opencontainers/runc/proto"
	"golang.org/x/net/context"
	"time"
	"log"
	"google.golang.org/grpc"
	"io/ioutil"
	"io"
	"os"
)

var migrationClientCommand = cli.Command{
	Name:  "migrationcli",
	Usage: "checkpoint a running container and transfer checkpoint files to migration server",
	ArgsUsage: `<container-id> <ip> <port>

Where "<container-id>" is the name for the instance of the container to be
checkpointed.`,
	Description: `The migration command saves the state of the container instance and transfer migration server.`,
	Flags: []cli.Flag{
		cli.StringFlag{Name: "image-path", Value: "", Usage: "path for saving criu image files"},
		cli.StringFlag{Name: "work-path", Value: "", Usage: "path for saving work files and logs"},
		cli.StringFlag{Name: "parent-path", Value: "", Usage: "path for previous criu image files in pre-dump"},
		cli.BoolFlag{Name: "leave-running", Usage: "leave the process running after checkpointing"},
		cli.BoolFlag{Name: "tcp-established", Usage: "allow open tcp connections"},
		cli.BoolFlag{Name: "ext-unix-sk", Usage: "allow external unix sockets"},
		cli.BoolFlag{Name: "shell-job", Usage: "allow shell jobs"},
		cli.BoolFlag{Name: "lazy-pages", Usage: "use userfaultfd to lazily restore memory pages"},
		cli.StringFlag{Name: "status-fd", Value: "", Usage: "criu writes \\0 to this FD once lazy-pages is ready"},
		cli.StringFlag{Name: "page-server", Value: "", Usage: "ADDRESS:PORT of the page server"},
		cli.BoolFlag{Name: "file-locks", Usage: "handle file locks, for safety"},
		cli.BoolFlag{Name: "pre-dump", Usage: "dump container's memory information only, leave the container running after this"},
		cli.StringFlag{Name: "manage-cgroups-mode", Value: "", Usage: "cgroups mode: 'soft' (default), 'full' and 'strict'"},
		cli.StringSliceFlag{Name: "empty-ns", Usage: "create a namespace, but don't restore its properties"},
		cli.BoolFlag{Name: "auto-dedup", Usage: "enable auto deduplication of memory images"},
		cli.BoolFlag{Name: "track-mem", Usage: "enable tracking memory for difference dump added by yota"},
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
			return fmt.Errorf("runc checkpoint requires root")
		}

		container, err := getContainer(context)
		if err != nil {
			return err
		}
		status, err := container.Status()
		if err != nil {
			return err
		}
		if status == libcontainer.Created || status == libcontainer.Stopped {
			fatalf("Container cannot be checkpointed in %s state", status.String())
		}
		defer destroy(container)
		options := criuOptions(context)
		// these are the mandatory criu options for a container
		setPageServer(context, options)
		setManageCgroupsMode(context, options)
		if err := setEmptyNsMask(context, options); err != nil {
			return err
		}
		if err := container.Checkpoint(options); err != nil {
			return err
		}

		//file transfer logic added by yota
		conn, err := grpc.Dial(context.Args().Get(1) + ":" + context.Args().Get(2), grpc.WithInsecure())
		if err != nil {
			return err
		}
		defer conn.Close()

		client := pb.NewFileTransferServiceClient(conn)

		ftsCli := &ftsClient{
			client: client,
		}

		folderpath := context.String("image-path")

		err = ftsCli.runGetFolderInfo(folderpath)
		if err != nil {
			return err
		}

		files, err := ioutil.ReadDir(folderpath)
		if err != nil {
			return err			
		}

		err = os.Chdir(folderpath)
		if err != nil {
			return err
		}

		for _, file := range files {
			err := ftsCli.runGetFileInfo(file.Name(), file.Size(), uint32(file.Mode()))
			if err != nil {
				return err
			}
			f, err := os.Open(file.Name())
			if err != nil {
				return err
			}
			buff := make([]byte, MAX_BUFF)
			for {
				count, err := f.Read(buff)
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				err = ftsCli.runTransferFile(buff[:count])
				if err != nil {
					return err
				}
			}
		}
		err = ftsCli.runRestoreContainer(context.Args().First())
		if err != nil {
			return err
		}
		return nil
	},
}

const (
	MAX_BUFF = 16384
)

type ftsClient struct {
	client pb.FileTransferServiceClient
}

func (fts *ftsClient)runGetFolderInfo(folderName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	folderInfo := &pb.FolderInfo{
		Name: folderName,
	}
	res, err := fts.client.GetFolderInfo(ctx, folderInfo)
	if err != nil {
		return err
	}
	log.Printf("return message: %s\n", res.Message)
	return nil
}

func (fts *ftsClient)runGetFileInfo(name string, size int64, mode uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fileInfo := &pb.FileInfo{
		Name: name,
		Mode: mode,
	}
	res, err := fts.client.GetFileInfo(ctx, fileInfo)
	if err != nil {
		return err
		
	}
	log.Printf("return message: %s\n", res.Message)
	return nil
}

func (fts *ftsClient)runTransferFile(data []byte) error {
	//log.Println(string(data))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := fts.client.TransferFile(ctx)
	for {
		fileData := &pb.FileData {
			Data: data,
			
		}
		if err := stream.Send(fileData);  err != nil {
			return err
			
		}
		break
		
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	log.Printf("return message: %s\n", res.Message)

	return nil	
} 

func (fts *ftsClient)runRestoreContainer(containerId string) error {
	log.Println("containerid:",containerId)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := fts.client.RestoreContainer(ctx, &pb.ContainerInfo{Name:containerId})
	log.Println(res.Message)
	if err != nil{
		return err
	}
	return nil
} 
