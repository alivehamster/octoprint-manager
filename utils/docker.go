package utils

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func createOctoPrintContainer(cli *client.Client, id string, device string, port int) (string, error) {
	portStr := strconv.Itoa(port)
	containerPort := "80/tcp"

	symlinkPath := "/dev/serial/by-id/" + device

	usb, err := filepath.EvalSymlinks(symlinkPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve device symlink %s: %w", symlinkPath, err)
	}

	config := &container.Config{
		Image: "octoprint/octoprint",
		ExposedPorts: nat.PortSet{
			nat.Port(containerPort): {},
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			nat.Port(containerPort): []nat.PortBinding{
				{
					HostPort: portStr,
				},
			},
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: fmt.Sprintf("/mnt/storage/octoprint/%s", id),
				Target: "/octoprint",
			},
		},
		Resources: container.Resources{
			Devices: []container.DeviceMapping{
				{
					PathOnHost:        usb,
					PathInContainer:   usb,
					CgroupPermissions: "rwm",
				},
			},
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	containerName := fmt.Sprintf("octoprint-%s", id)

	resp, err := cli.ContainerCreate(
		context.Background(),
		config,
		hostConfig,
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	err = cli.ContainerStart(context.Background(), resp.ID, container.StartOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	return containerName, nil
}

func getNextAvailablePort(db *sql.DB) (int, error) {
	var maxPort sql.NullInt64
	err := db.QueryRow("SELECT MAX(port) FROM containers").Scan(&maxPort)
	if err != nil {
		return 0, fmt.Errorf("failed to query max port: %w", err)
	}

	if !maxPort.Valid {
		return 2000, nil
	}

	return int(maxPort.Int64) + 1, nil
}

func CreateNewContainer(cli *client.Client, db *sql.DB, device string) (string, error) {
	id := uuid.New().String()
	port, err := getNextAvailablePort(db)
	if err != nil {
		return "", fmt.Errorf("failed to get next available port: %w", err)
	}

	volumePath := fmt.Sprintf("/mnt/storage/octoprint/%s", id)
	if err := os.MkdirAll(volumePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create volume directory: %w", err)
	}

	containerName, err := createOctoPrintContainer(cli, id, device, port)
	if err != nil {
		return "", err
	}

	_, err = db.Exec("INSERT INTO containers (id, device, port) VALUES (?, ?, ?)", id, device, port)
	if err != nil {
		return "", fmt.Errorf("failed to insert new container into database: %w", err)
	}

	return containerName, nil
}

func RecreateAllContainers(cli *client.Client, db *sql.DB) error {
	rows, err := db.Query("SELECT id, device, port FROM containers")
	if err != nil {
		return fmt.Errorf("failed to query containers: %w", err)
	}
	defer rows.Close()

	var errors []string
	for rows.Next() {
		var id, device string
		var port int

		if err := rows.Scan(&id, &device, &port); err != nil {
			errors = append(errors, fmt.Sprintf("failed to scan row: %v", err))
			continue
		}

		containerName := fmt.Sprintf("octoprint-%s", id)

		// Check if container exists
		containerJSON, err := cli.ContainerInspect(context.Background(), containerName)
		if err == nil {
			if containerJSON.State.Running {
				continue
			}
			err = cli.ContainerStart(context.Background(), containerName, container.StartOptions{})
			if err != nil {
				errors = append(errors, fmt.Sprintf("failed to start existing container %s: %v", containerName, err))
			}
			continue
		}

		_, err = createOctoPrintContainer(cli, id, device, port)
		if err != nil {
			errors = append(errors, fmt.Sprintf("failed to create container %s: %v", containerName, err))
			continue
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating rows: %w", err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("encountered errors while starting containers: %v", errors)
	}

	return nil
}

func EnsureOctoPrintImage(cli *client.Client) error {
	imageName := "octoprint/octoprint:latest"

	_, err := cli.ImageInspect(context.Background(), imageName)
	if err == nil {
		return nil
	}

	fmt.Println("Pulling octoprint/octoprint image...")
	reader, err := cli.ImagePull(context.Background(), imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer reader.Close()

	_, err = io.Copy(os.Stdout, reader)
	if err != nil {
		return fmt.Errorf("failed to read pull output: %w", err)
	}

	fmt.Println("Successfully pulled octoprint/octoprint image")
	return nil
}

func DeleteContainer(c *fiber.Ctx, cli *client.Client, db *sql.DB, id string) error {
	containerName := fmt.Sprintf("octoprint-%s", id)

	// Stop the container
	ctx := context.Background()
	timeout := 10
	stopOptions := container.StopOptions{
		Timeout: &timeout,
	}
	if err := cli.ContainerStop(ctx, containerName, stopOptions); err != nil {
		log.Println("Failed to stop container (may not exist):", err)
	}

	// Remove the container
	removeOptions := container.RemoveOptions{
		Force: true,
	}
	if err := cli.ContainerRemove(ctx, containerName, removeOptions); err != nil {
		log.Println("Failed to remove container:", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to remove container",
		})
	}

	// Delete from database
	_, err := db.Exec("DELETE FROM containers WHERE id = ?", id)
	if err != nil {
		log.Println("Failed to delete container from database:", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to delete container from database",
		})
	}

	// Delete the storage directory
	volumePath := fmt.Sprintf("/mnt/storage/octoprint/%s", id)
	if err := os.RemoveAll(volumePath); err != nil {
		log.Println("Failed to delete volume directory:", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "Failed to delete volume directory",
		})
	}
	return nil
}
