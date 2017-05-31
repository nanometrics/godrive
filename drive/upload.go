package drive

import (
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

type UploadArgs struct {
	Out         io.Writer
	Progress    io.Writer
	Path        string
	Name        string
	Description string
	Parents     []string
	Folder      string
	Mime        string
	Recursive   bool
	Share       bool
	Delete      bool
	ChunkSize   int64
	Timeout     time.Duration
}

func (self *Drive) Upload(args UploadArgs) error {
	if args.ChunkSize > intMax()-1 {
		return fmt.Errorf("Chunk size is to big, max chunk size for this computer is %d", intMax()-1)
	}

	if args.Folder != "" {
		parentId, err := self.parentFromFolder(args.Folder)
		if err != nil {
			return err
		}
		args.Parents = []string{parentId}
	}

	// Ensure that none of the parents are sync dirs
	for _, parent := range args.Parents {
		isSyncDir, err := self.isSyncFile(parent)
		if err != nil {
			return err
		}

		if isSyncDir {
			return fmt.Errorf("%s is a sync directory, use 'sync upload' instead", parent)
		}
	}

	if args.Recursive {
		return self.uploadRecursive(args)
	}

	info, err := os.Stat(args.Path)
	if err != nil {
		return fmt.Errorf("Failed stat file: %s", err)
	}

	if info.IsDir() {
		return fmt.Errorf("'%s' is a directory, use --recursive to upload directories", info.Name())
	}

	f, rate, err := self.uploadFile(args)
	if err != nil {
		return err
	}
	fmt.Fprintf(args.Out, "Uploaded %s at %s/s, total %s\n", f.Id, formatSize(rate, false), formatSize(f.Size, false))

	if args.Share {
		err = self.shareAnyoneReader(f.Id)
		if err != nil {
			return err
		}

		fmt.Fprintf(args.Out, "File is readable by anyone at %s\n", f.WebContentLink)
	}

	if args.Delete {
		err = os.Remove(args.Path)
		if err != nil {
			return fmt.Errorf("Failed to delete file: %s", err)
		}
		fmt.Fprintf(args.Out, "Removed %s\n", args.Path)
	}

	return nil
}

func (self *Drive) uploadRecursive(args UploadArgs) error {
	info, err := os.Stat(args.Path)
	if err != nil {
		return fmt.Errorf("Failed stat file: %s", err)
	}
	if info.IsDir() {
		args.Name = ""
		err = self.uploadDirectory(args)
	} else if info.Mode().IsRegular() {
		_, _, err = self.uploadFile(args)
	}
	if err != nil {
		return err
	}
	if args.Delete {
		err = os.Remove(args.Path)
		if err != nil {
			return fmt.Errorf("Failed to remove: %s", err)
		}
		fmt.Fprintf(args.Out, "Removed %s\n", args.Path)
	}
	return nil
}

func (self *Drive) uploadDirectory(args UploadArgs) error {
	srcFile, srcFileInfo, err := openFile(args.Path)
	if err != nil {
		return err
	}
	// Close file on function exit
	defer srcFile.Close()

	// check if directory exists
	id, err := self.existingFolderId(args.Parents[0], srcFileInfo.Name())
	if err != nil {
		return err
	}
	if id == "" {
		fmt.Fprintf(args.Out, "Creating directory %s\n", srcFileInfo.Name())
		// Make directory on drive
		f, err := self.mkdir(MkdirArgs{
			Out:         args.Out,
			Name:        srcFileInfo.Name(),
			Parents:     args.Parents,
			Description: args.Description,
		})
		if err != nil {
			return err
		}
		id = f.Id
	}

	// Read files from directory
	names, err := srcFile.Readdirnames(0)
	if err != nil && err != io.EOF {
		return fmt.Errorf("Failed reading directory: %s", err)
	}

	for _, name := range names {
		// Copy args and set new path and parents
		newArgs := args
		newArgs.Path = filepath.Join(args.Path, name)
		newArgs.Parents = []string{id}
		newArgs.Description = ""

		// Upload
		err = self.uploadRecursive(newArgs)
		if err != nil {
			return err
		}
	}

	return nil
}

func (self *Drive) uploadFile(args UploadArgs) (*drive.File, int64, error) {
	md5Channel := make(chan string)
	go func() {
		md5Channel <- Md5sum(args.Path)
	}()
	srcFile, srcFileInfo, err := openFile(args.Path)
	if err != nil {
		return nil, 0, err
	}

	// Close file on function exit
	defer srcFile.Close()

	// Instantiate empty drive file
	dstFile := &drive.File{Description: args.Description}

	// Use provided file name or use filename
	if args.Name == "" {
		dstFile.Name = filepath.Base(srcFileInfo.Name())
	} else {
		dstFile.Name = args.Name
	}

	// Set provided mime type or get type based on file extension
	if args.Mime == "" {
		dstFile.MimeType = mime.TypeByExtension(filepath.Ext(dstFile.Name))
	} else {
		dstFile.MimeType = args.Mime
	}

	// Set parent folders
	dstFile.Parents = args.Parents

	// if file exists with same name and checksum, skip upload
	existingFile, err := self.existingFile(dstFile.Parents[0], dstFile.Name)
	if err != nil {
		return nil, 0, err
	}
	if existingFile != nil {
		localMd5 := <-md5Channel
		if localMd5 == existingFile.Md5Checksum {
			return existingFile, 0, nil
		}
	}

	// Chunk size option
	chunkSize := googleapi.ChunkSize(int(args.ChunkSize))

	// Wrap file in progress reader
	progressReader := getProgressReader(srcFile, args.Progress, srcFileInfo.Size())

	// Wrap reader in timeout reader
	reader, ctx := getTimeoutReaderContext(progressReader, args.Timeout)

	fmt.Fprintf(args.Out, "Uploading %s\n", args.Path)
	started := time.Now()

	f, err := self.service.Files.Create(dstFile).SupportsTeamDrives(true).Fields("id", "name", "size", "md5Checksum", "webContentLink").Context(ctx).Media(reader, chunkSize).Do()
	if err != nil {
		if isTimeoutError(err) {
			return nil, 0, fmt.Errorf("Failed to upload file: timeout, no data was transferred for %v", args.Timeout)
		}
		return nil, 0, fmt.Errorf("Failed to upload file: %s", err)
	}
	localMd5 := <-md5Channel
	if f.Md5Checksum != localMd5 {
		return nil, 0, fmt.Errorf("Failed to verify uploaded file %s from %s, local checksum %s, remote checksum %s", f.Id, args.Path, localMd5, f.Md5Checksum)
	}

	// Calculate average upload rate
	rate := calcRate(f.Size, started, time.Now())

	return f, rate, nil
}

type UploadStreamArgs struct {
	Out         io.Writer
	In          io.Reader
	Name        string
	Description string
	Parents     []string
	Mime        string
	Share       bool
	ChunkSize   int64
	Progress    io.Writer
	Timeout     time.Duration
}

func (self *Drive) UploadStream(args UploadStreamArgs) error {
	if args.ChunkSize > intMax()-1 {
		return fmt.Errorf("Chunk size is to big, max chunk size for this computer is %d", intMax()-1)
	}

	// Instantiate empty drive file
	dstFile := &drive.File{Name: args.Name, Description: args.Description}

	// Set mime type if provided
	if args.Mime != "" {
		dstFile.MimeType = args.Mime
	}

	// Set parent folders
	dstFile.Parents = args.Parents

	// Chunk size option
	chunkSize := googleapi.ChunkSize(int(args.ChunkSize))

	// Wrap file in progress reader
	progressReader := getProgressReader(args.In, args.Progress, 0)

	// Wrap reader in timeout reader
	reader, ctx := getTimeoutReaderContext(progressReader, args.Timeout)

	fmt.Fprintf(args.Out, "Uploading %s\n", dstFile.Name)
	started := time.Now()

	f, err := self.service.Files.Create(dstFile).SupportsTeamDrives(true).Fields("id", "name", "size", "webContentLink").Context(ctx).Media(reader, chunkSize).Do()
	if err != nil {
		if isTimeoutError(err) {
			return fmt.Errorf("Failed to upload file: timeout, no data was transferred for %v", args.Timeout)
		}
		return fmt.Errorf("Failed to upload file: %s", err)
	}

	// Calculate average upload rate
	rate := calcRate(f.Size, started, time.Now())

	fmt.Fprintf(args.Out, "Uploaded %s at %s/s, total %s\n", f.Id, formatSize(rate, false), formatSize(f.Size, false))
	if args.Share {
		err = self.shareAnyoneReader(f.Id)
		if err != nil {
			return err
		}

		fmt.Fprintf(args.Out, "File is readable by anyone at %s\n", f.WebContentLink)
	}
	return nil
}

func (self *Drive) parentFromFolder(folderPath string) (string, error) {
	// fmt.Printf("Looking for folder path %s\n", folderPath)
	parts := strings.Split(folderPath, "/")
	parentId := ""
	for _, name := range parts {
		if name == "" {
			continue
		}
		// fmt.Printf("Looking for %s\n", name)
		if parentId == "" {
			if strings.EqualFold("mydrive", strings.Replace(name, " ", "", -1)) {
				parentId = "root"
			} else {
				// fmt.Printf("Looking for team drive %s\n", name)
				result, err := self.service.Teamdrives.List().Fields("teamDrives(id,name)").Do()
				if err != nil {
					return "", err
				}
				for _, drive := range result.TeamDrives {
					// fmt.Printf("Checking team drive %s\n", drive.Name)
					if drive.Name == name {
						parentId = drive.Id
						break
					}
				}
			}
			if parentId == "" {
				return "", fmt.Errorf("No top level folder matched name %s", name)
			}
			// fmt.Printf("Found parent %s for %s\n", parentId, name)
		} else {
			query := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and name = '%s' and '%s' in parents", name, parentId)
			// fmt.Printf("Query: %s\n", query)
			result, err := self.service.Files.List().SupportsTeamDrives(true).IncludeTeamDriveItems(true).Q(query).Fields("files(id,name)").Do()
			if err != nil {
				return "", err
			}
			if len(result.Files) == 0 {
				return "", fmt.Errorf("No folders matched name %s", name)
			}
			parentId = result.Files[0].Id
			// fmt.Printf("Found parent %s for %s\n", parentId, name)
		}
	}
	// fmt.Printf("Parent %s for %s\n", parentId, folderPath)
	return parentId, nil
}

func (self *Drive) existingFolderId(parentId string, name string) (string, error) {
	query := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and name = '%s' and '%s' in parents", name, parentId)
	result, err := self.service.Files.List().SupportsTeamDrives(true).IncludeTeamDriveItems(true).Q(query).Fields("files(id,name)").Do()
	if err != nil {
		return "", err
	}
	if len(result.Files) == 0 {
		return "", nil
	}
	folderId := result.Files[0].Id
	return folderId, nil
}

func (self *Drive) existingFile(parentId string, name string) (*drive.File, error) {
	query := fmt.Sprintf("name = '%s' and '%s' in parents", name, parentId)
	result, err := self.service.Files.List().SupportsTeamDrives(true).IncludeTeamDriveItems(true).Q(query).Fields("files(id,name,md5Checksum)").Do()
	if err != nil {
		return nil, err
	}
	if len(result.Files) == 0 {
		return nil, nil
	}
	return result.Files[0], nil
}
