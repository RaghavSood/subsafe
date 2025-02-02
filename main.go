package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type ChainConfig struct {
	Name           string `json:"name"`
	ChainID        int64  `json:"chain_id"`
	RPC            string `json:"rpc_url"`
	ExplorerURL    string `json:"explorer_url"`
	SafeServiceURL string `json:"safe_service_url"`
}

type SafeTransactionResponse struct {
	Safe            string `json:"safe"`
	To              string `json:"to"`
	Proposer        string `json:"proposer"`
	TransactionHash string `json:"transactionHash"`
	IsExecuted      bool   `json:"isExecuted"`
	IsSuccessful    bool   `json:"isSuccessful"`
	Confirmations   []struct {
		Owner          string    `json:"owner"`
		SubmissionDate time.Time `json:"submissionDate"`
		SignatureType  string    `json:"signatureType"`
	} `json:"confirmations"`
}

type Config struct {
	TelegramToken string            `json:"telegram_token"`
	ChatID        int64             `json:"chat_id"`
	SafeAddresses map[string]string `json:"safe_addresses"`
	SignerLabels  map[string]string `json:"signer_labels"`
	Chains        []ChainConfig     `json:"chains"`
}

type Monitor struct {
	config     Config
	bot        *tgbotapi.BotAPI
	safeABI    abi.ABI
	wg         sync.WaitGroup
	ctx        context.Context
	cancelFunc context.CancelFunc
}

func loadConfig(path string) (Config, error) {
	var config Config
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("reading config file: %w", err)
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parsing config: %w", err)
	}

	// Convert all signer addresses to lowercase for case-insensitive matching
	normalizedLabels := make(map[string]string)
	for addr, label := range config.SignerLabels {
		normalizedLabels[strings.ToLower(addr)] = label
	}
	config.SignerLabels = normalizedLabels

	return config, nil
}

func getSafeTransactionDetails(serviceURL, txHash string) (*SafeTransactionResponse, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	url := fmt.Sprintf("%s/api/v1/multisig-transactions/%s/", serviceURL, txHash)

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to call Safe API: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Safe API returned non-200 status: %d, body: %s", resp.StatusCode, string(body))
	}

	var result SafeTransactionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode Safe API response: %w", err)
	}

	return &result, nil
}

func NewMonitor(config Config) (*Monitor, error) {
	bot, err := tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("creating telegram bot: %w", err)
	}

	parsedABI, err := abi.JSON(strings.NewReader(gnosisSafeABI))
	if err != nil {
		return nil, fmt.Errorf("parsing ABI: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Monitor{
		config:     config,
		bot:        bot,
		safeABI:    parsedABI,
		ctx:        ctx,
		cancelFunc: cancel,
	}, nil
}

func (m *Monitor) getSignerLabel(address string) string {
	if label, ok := m.config.SignerLabels[strings.ToLower(address)]; ok {
		return label
	}
	// If no label found, return shortened address
	return address[:6]
}

func (m *Monitor) sendMessage(msg string) {
	teleMsg := tgbotapi.NewMessage(m.config.ChatID, msg)
	teleMsg.ParseMode = "MarkdownV2"
	teleMsg.DisableWebPagePreview = true
	if _, err := m.bot.Send(teleMsg); err != nil {
		log.Printf("Error sending telegram message: %v", err)
	}
}

func (m *Monitor) monitorChain(chainConfig ChainConfig, addresses []common.Address, addressLabels map[common.Address]string) {
	defer m.wg.Done()

	const (
		initialRetryDelay = 1 * time.Second
		maxRetryDelay     = 60 * time.Second
		backoffFactor     = 2
		maxFailedAttempts = 3
	)

	retryDelay := initialRetryDelay
	firstConnect := true
	attemptCount := 0
	alertSent := false

	for {
		if !firstConnect {
			time.Sleep(retryDelay)
			retryDelay *= backoffFactor
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}

		if err := m.ctx.Err(); err != nil {
			return // Context cancelled
		}

		if firstConnect || (attemptCount >= maxFailedAttempts) {
			chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
			m.sendMessage(fmt.Sprintf("🔄 Connecting to *%s* \\(Chain ID: %d\\)", chainName, chainConfig.ChainID))
		}

		client, err := ethclient.Dial(chainConfig.RPC)
		if err != nil {
			attemptCount++
			if attemptCount >= maxFailedAttempts && !alertSent {
				errorMsg := strings.ReplaceAll(err.Error(), ".", "\\.")
				chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
				m.sendMessage(fmt.Sprintf("⚠️ *ALERT*: %s connection has failed %d times in a row\\. This could indicate a serious connectivity issue\\.\nLast error: %s",
					chainName, attemptCount, errorMsg))
				alertSent = true
			} else {
				m.sendMessage(fmt.Sprintf("❌ Failed to connect to %s: %v. Retrying in %v...",
					chainConfig.Name, err, retryDelay))
			}
			continue
		}

		logs := make(chan types.Log)
		sub, err := client.SubscribeFilterLogs(m.ctx, ethereum.FilterQuery{
			Addresses: addresses,
		}, logs)

		if err != nil {
			attemptCount++
			if attemptCount >= maxFailedAttempts && !alertSent {
				m.sendMessage(fmt.Sprintf("⚠️ ALERT: %s connection has failed %d times in a row. This could indicate a serious connectivity issue.\nLast error: %v",
					chainConfig.Name, attemptCount, err))
				alertSent = true
			} else {
				m.sendMessage(fmt.Sprintf("❌ Failed to subscribe to logs on %s: %v. Retrying in %v...",
					chainConfig.Name, err, retryDelay))
			}
			client.Close()
			continue
		}

		if firstConnect || alertSent {
			chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
			m.sendMessage(fmt.Sprintf("🟢 Successfully connected to *%s*", chainName))
		}
		retryDelay = initialRetryDelay
		attemptCount = 0
		alertSent = false
		firstConnect = false

		func() {
			defer client.Close()
			defer sub.Unsubscribe()

			for {
				select {
				case err := <-sub.Err():
					log.Printf("Error from subscription: %v", err)
					return
				case vLog := <-logs:
					event, err := m.safeABI.EventByID(vLog.Topics[0])
					if err != nil {
						log.Printf("Error parsing event: %v", err)
						continue
					}

					txHash := vLog.TxHash.Hex()
					walletLabel := addressLabels[vLog.Address]

					// Escape special characters for MarkdownV2
					chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
					eventName := strings.ReplaceAll(event.Name, ".", "\\.")
					escapedLabel := strings.ReplaceAll(walletLabel, ".", "\\.")
					address := strings.ReplaceAll(vLog.Address.Hex(), ".", "\\.")
					txHashEscaped := strings.ReplaceAll(txHash, ".", "\\.")
					explorerURL := strings.ReplaceAll(fmt.Sprintf("%s/tx/%s", chainConfig.ExplorerURL, txHash), ".", "\\.")

					var signerInfo string
					if eventName == "ExecutionSuccess" && len(vLog.Topics) > 1 {
						safeTxHash := vLog.Topics[1].Hex()
						if chainConfig.SafeServiceURL != "" {
							if txDetails, err := getSafeTransactionDetails(chainConfig.SafeServiceURL, safeTxHash); err != nil {
								signerInfo = "\n\\(Safe API call failed\\)"
							} else {
								// Get proposer label or shortened address
								proposerLabel := m.getSignerLabel(txDetails.Proposer)
								proposerLabel = strings.ReplaceAll(proposerLabel, ".", "\\.")
								propURL := strings.ReplaceAll(fmt.Sprintf("%s/address/%s", chainConfig.ExplorerURL, txDetails.Proposer), ".", "\\.")
								proposerLabel = fmt.Sprintf("[%s](%s)", proposerLabel, propURL)

								signers := make([]string, 0, len(txDetails.Confirmations))
								for _, conf := range txDetails.Confirmations {
									signerLabel := m.getSignerLabel(conf.Owner)
									signerLabel = strings.ReplaceAll(signerLabel, ".", "\\.")
									signerURL := strings.ReplaceAll(fmt.Sprintf("%s/address/%s", chainConfig.ExplorerURL, conf.Owner), ".", "\\.")
									signers = append(signers, fmt.Sprintf("[%s](%s)", signerLabel, signerURL))
								}
								signerInfo = fmt.Sprintf("\n*Proposer:* %s\n*Signers:* %s",
									proposerLabel, strings.Join(signers, ", "))
							}
						}
					}

					msg := fmt.Sprintf("🔔 *New event on %s*\n"+
						"*Wallet:* %s\n"+
						"*Type:* %s\n"+
						"*Address:* `%s`\n"+
						"*Tx:* [%s](%s)%s",
						chainName, escapedLabel, eventName, address, txHashEscaped, explorerURL, signerInfo)
					m.sendMessage(msg)
				case <-m.ctx.Done():
					return
				}
			}
		}()
	}
}

func (m *Monitor) Start() error {
	addresses := make([]common.Address, 0, len(m.config.SafeAddresses))
	addressLabels := make(map[common.Address]string)

	for label, addr := range m.config.SafeAddresses {
		address := common.HexToAddress(addr)
		addresses = append(addresses, address)
		addressLabels[address] = label
	}

	for _, chainConfig := range m.config.Chains {
		m.wg.Add(1)
		go m.monitorChain(chainConfig, addresses, addressLabels)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	m.sendMessage("⚠️ Bot is shutting down")
	m.cancelFunc()
	m.wg.Wait()

	return nil
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("Usage: ./safe-monitor config.json")
	}

	config, err := loadConfig(os.Args[1])
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	monitor, err := NewMonitor(config)
	if err != nil {
		log.Fatalf("Error creating monitor: %v", err)
	}

	if err := monitor.Start(); err != nil {
		log.Fatalf("Error running monitor: %v", err)
	}
}
