package tasmotahomekit

import (
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

const (
	// Eve Energy Service
	// Source: https://github.com/simont77/fakegato-history
	// Source: https://gist.github.com/gomfunkel/b1a046d729757120907c
	TypeEveEnergyService = "E863F007-079E-48FF-8F27-9C2605A29F52"

	// Eve Characteristics
	TypeEveCurrentConsumption = "E863F10D-079E-48FF-8F27-9C2605A29F52" // Watts
	TypeEveTotalConsumption   = "E863F10C-079E-48FF-8F27-9C2605A29F52" // kWh
	TypeEveVoltage            = "E863F10A-079E-48FF-8F27-9C2605A29F52" // Volts
	TypeEveCurrent            = "E863F126-079E-48FF-8F27-9C2605A29F52" // Amperes
)

// EveEnergyService is a custom service for Eve Energy devices
type EveEnergyService struct {
	*service.S

	CurrentConsumption *characteristic.Float
	TotalConsumption   *characteristic.Float
	Voltage            *characteristic.Float
	Current            *characteristic.Float
}

// NewEveEnergyService creates a new Eve Energy service
func NewEveEnergyService() *EveEnergyService {
	s := EveEnergyService{}
	s.S = service.New(TypeEveEnergyService)

	s.CurrentConsumption = characteristic.NewFloat(TypeEveCurrentConsumption)
	s.CurrentConsumption.SetMinValue(0)
	s.CurrentConsumption.SetMaxValue(10000)
	s.CurrentConsumption.SetStepValue(0.1)
	s.CurrentConsumption.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	s.AddC(s.CurrentConsumption.C)

	s.TotalConsumption = characteristic.NewFloat(TypeEveTotalConsumption)
	s.TotalConsumption.SetMinValue(0)
	s.TotalConsumption.SetMaxValue(1000000)
	s.TotalConsumption.SetStepValue(0.001)
	s.TotalConsumption.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	s.AddC(s.TotalConsumption.C)

	s.Voltage = characteristic.NewFloat(TypeEveVoltage)
	s.Voltage.SetMinValue(0)
	s.Voltage.SetMaxValue(500)
	s.Voltage.SetStepValue(0.1)
	s.Voltage.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	s.AddC(s.Voltage.C)

	s.Current = characteristic.NewFloat(TypeEveCurrent)
	s.Current.SetMinValue(0)
	s.Current.SetMaxValue(100)
	s.Current.SetStepValue(0.01)
	s.Current.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	s.AddC(s.Current.C)

	return &s
}
