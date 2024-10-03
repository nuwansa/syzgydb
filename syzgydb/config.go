package main

import (
	"fmt"

	"github.com/smhanov/syzgydb"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func init() {
	// Bind command-line flags
	pflag.String("ollama-server", "", "Hostname and port of the Ollama server")
	pflag.String("text-model", "", "Name of the text embedding model")
	pflag.String("image-model", "", "Name of the image embedding model")
	pflag.String("config", "", "Path to the configuration file")
	pflag.String("data-folder", "./data", "Path to the data folder")
}

func LoadConfig() error {
	// Set default values
	viper.SetDefault("ollama_server", "localhost:11434")
	viper.SetDefault("text_model", "all-minilm")
	viper.SetDefault("image_model", "minicpm-v")
	viper.SetDefault("data_folder", "./data")

	// Bind command-line flags to Viper
	viper.BindPFlags(pflag.CommandLine)

	// Parse command-line flags
	pflag.Parse()

	// Bind environment variables
	viper.AutomaticEnv()

	// Read configuration file if specified
	configFile := viper.GetString("config")
	if configFile != "" {
		viper.SetConfigFile(configFile)
	} else {
		viper.SetConfigName("syzgy.conf")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/syzgydb/")
	}

	if err := viper.ReadInConfig(); err != nil {
		fmt.Printf("Using defaults and command line/environent options\n     (%v)\n", err)
	}

	// Unmarshal configuration into struct
	var cfg syzgydb.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("unable to decode into struct, %v", err)
	}

	// Ensure the data folder exists
	dataFolder := viper.GetString("data_folder")
	if _, err := os.Stat(dataFolder); os.IsNotExist(err) {
		if err := os.MkdirAll(dataFolder, os.ModePerm); err != nil {
			return fmt.Errorf("failed to create data folder: %v", err)
		}
	}
	fmt.Println("Configuration values:")
	fmt.Printf("Ollama Server: %s\n", viper.GetString("ollama_server"))
	fmt.Printf("Text Model: %s\n", viper.GetString("text_model"))
	fmt.Printf("Image Model: %s\n", viper.GetString("image_model"))
	fmt.Printf("Data Folder: %s\n", viper.GetString("data_folder"))

	// Assign the loaded configuration to the global variable
	syzgydb.Configure(cfg)

	return nil
}
