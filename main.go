package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	_ "github.com/mattn/go-sqlite3"
	"nxweb.com/octoprint-manager/utils"
)

func main() {

	path := "./config"

	if len(os.Args) == 2 {
		path = os.Args[1]
	}

	configdir, err := filepath.Abs(path)
	if err != nil {
		log.Fatal("Failed to resolve config directory path:", err)
	}

	if err := os.MkdirAll(configdir, 0755); err != nil {
		log.Fatal("Failed to create config directory:", err)
	}

	db, err := sql.Open("sqlite3", configdir+"/octoprint.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}
	defer db.Close()

	err = db.Ping()
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	log.Println("Database connected successfully")

	createTableSQL := `CREATE TABLE IF NOT EXISTS containers (
        id TEXT PRIMARY KEY,
        device TEXT NOT NULL,
        port INTEGER NOT NULL,
        name TEXT
    );`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Failed to create containers table:", err)
	}

	log.Println("Containers table ready")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatal("failed to create docker client:", err)
	}
	defer cli.Close()

	err = utils.EnsureOctoPrintImage(cli)
	if err != nil {
		log.Fatal("failed to ensure octoprint image:", err)
	}

	err, containerStatus := utils.RecreateAllContainers(cli, db, configdir)
	if err != nil {
		if containerStatus == nil {
			log.Fatal("failed to recreate containers:", err)
		}
		log.Println("Some containers failed to start:", err)
	}

	app := fiber.New()
	app.Static("/", "./frontend")

	app.Get("/api/screenoff", func(c *fiber.Ctx) error {

		cmd := exec.Command("bash", "./screenoff.sh")

		err := cmd.Run()

		if err != nil {
			fmt.Println("Screen off command failed:", err)
			return c.Status(500).SendString("Failed to turn off screen")
		}

		return c.SendString("Screen off command executed")
	})

	app.Get("/api/reboot", func(c *fiber.Ctx) error {

		cmd := exec.Command("reboot")

		err := cmd.Run()

		if err != nil {
			fmt.Println("Command failed:", err)
			return c.Status(500).SendString("Failed to reboot")
		}

		return c.SendString("Reboot command executed")
	})

	app.Get("/api/shutdown", func(c *fiber.Ctx) error {

		cmd := exec.Command("shutdown", "now")

		err := cmd.Run()

		if err != nil {
			fmt.Println("Command failed:", err)
			return c.Status(500).SendString("Failed to shutdown")
		}

		return c.SendString("Shutdown command executed")
	})

	app.Get("/api/listusb", func(c *fiber.Ctx) error {

		if _, err := os.Stat("/dev/serial"); os.IsNotExist(err) {
			return c.JSON(fiber.Map{
				"error":   true,
				"message": "No USB Devices plugged in",
			})
		}

		files, err := os.ReadDir("/dev/serial/by-id")
		if err != nil {
			log.Println("Failed to read /dev/serial/by-id:", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to list USB devices",
			})
		}

		// Get devices currently in use
		rows, err := db.Query("SELECT device FROM containers")
		if err != nil {
			log.Println("Failed to query devices from database", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to query used devices",
			})
		}
		defer rows.Close()

		usedDevices := make(map[string]bool)
		for rows.Next() {
			var device string
			if err := rows.Scan(&device); err != nil {
				log.Println("Failed to scan device:", err)
				continue
			}
			usedDevices[device] = true
		}

		type DeviceInfo struct {
			Name  string `json:"name"`
			InUse bool   `json:"inUse"`
		}

		devices := make([]DeviceInfo, 0, len(files))
		for _, file := range files {
			if !file.IsDir() {
				devices = append(devices, DeviceInfo{
					Name:  file.Name(),
					InUse: usedDevices[file.Name()],
				})
			}
		}

		return c.JSON(fiber.Map{
			"error":   false,
			"devices": devices,
		})
	})

	app.Post("/api/newcontainer", func(c *fiber.Ctx) error {

		type Request struct {
			Device string `json:"device"`
		}

		var req Request
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}
		containerName, err := utils.CreateNewContainer(cli, db, req.Device, configdir)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": fmt.Sprintf("Failed to create container: %v", err),
			})
		}

		containerStatus[containerName[len("octoprint-"):]] = true

		return c.JSON(fiber.Map{
			"error":         false,
			"containerName": containerName,
		})
	})

	app.Get("/api/getcontainers", func(c *fiber.Ctx) error {
		rows, err := db.Query("SELECT id, port, name, device FROM containers")
		if err != nil {
			log.Println("Failed to query containers:", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to query containers",
			})
		}
		defer rows.Close()

		type ContainerInfo struct {
			ID     string  `json:"id"`
			Port   int     `json:"port"`
			Name   *string `json:"name"`
			Device string  `json:"device"`
			Status bool    `json:"status"`
		}

		containers := make([]ContainerInfo, 0)
		for rows.Next() {
			var id string
			var port int
			var name *string
			var device string

			if err := rows.Scan(&id, &port, &name, &device); err != nil {
				log.Println("Failed to scan container row:", err)
				continue
			}
			containers = append(containers, ContainerInfo{
				ID:     id,
				Port:   port,
				Name:   name,
				Device: device,
				Status: containerStatus[id],
			})
		}

		if err := rows.Err(); err != nil {
			log.Println("Error iterating container rows:", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Error reading containers",
			})
		}

		return c.JSON(fiber.Map{
			"error":      false,
			"containers": containers,
		})
	})

	app.Post("/api/deletecontainer", func(c *fiber.Ctx) error {

		type Request struct {
			Id string `json:"id"`
		}

		var req Request
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}

		utils.DeleteContainer(c, cli, db, req.Id, configdir)

		return c.JSON(fiber.Map{
			"error": false,
			"id":    req.Id,
		})
	})

	app.Post("/api/restartcontainer", func(c *fiber.Ctx) error {

		type Request struct {
			Id string `json:"id"`
		}

		var req Request
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}

		containerName := "octoprint-" + req.Id

		_, err := cli.ContainerInspect(context.Background(), containerName)
		if err == nil {
			err := cli.ContainerRestart(context.Background(), containerName, container.StopOptions{})
			if err != nil {
				log.Println("Failed to restart container:", err)
				containerStatus[req.Id] = false
				return c.Status(500).JSON(fiber.Map{
					"error":  "Failed to restart container",
					"status": false,
				})
			}
		} else {
			var device string
			var port int
			err := db.QueryRow("SELECT device, port FROM containers WHERE id = ?", req.Id).Scan(&device, &port)
			if err != nil {
				log.Println("Failed to get container info from database:", err)
				containerStatus[req.Id] = false
				return c.Status(500).JSON(fiber.Map{
					"error":  "Failed to get container info",
					"status": false,
				})
			}

			_, err = utils.CreateOctoPrintContainer(cli, req.Id, device, port, configdir)
			if err != nil {
				log.Println("Failed to recreate container:", err)
				containerStatus[req.Id] = false
				return c.Status(500).JSON(fiber.Map{
					"error":  "Failed to recreate container",
					"status": false,
				})
			}
		}

		containerStatus[req.Id] = true

		return c.JSON(fiber.Map{
			"error": false,
			"id":    req.Id,
		})

	})

	app.Post("/api/renamecontainer", func(c *fiber.Ctx) error {

		type Request struct {
			Id   string `json:"id"`
			Name string `json:"name"`
		}

		var req Request
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}

		_, err := db.Exec("UPDATE containers SET name = ? WHERE id = ?", req.Name, req.Id)
		if err != nil {
			log.Println("Failed to rename container:", err)
			return c.Status(500).JSON(fiber.Map{
				"error": "Failed to rename container",
			})
		}

		return c.JSON(fiber.Map{
			"error": false,
			"id":    req.Id,
			"name":  req.Name,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	log.Println("Server starting on", port)
	log.Fatal(app.Listen(":" + port))
}
