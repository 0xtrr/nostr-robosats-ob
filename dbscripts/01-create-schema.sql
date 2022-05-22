CREATE DATABASE IF NOT EXISTS robosatsob;

DROP TABLE IF EXISTS `orders`;

CREATE TABLE IF NOT EXISTS `robosatsob`.`orders` (
  `orderId` INT NOT NULL,
  PRIMARY KEY (`orderId`),
  UNIQUE INDEX `orderId_UNIQUE` (`orderId` ASC) VISIBLE)
ENGINE = InnoDB;
