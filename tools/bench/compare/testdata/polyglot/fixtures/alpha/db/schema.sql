-- MySQL fixture: declared only; never applied to a database.
CREATE PROCEDURE refresh_item() BEGIN SET @q = CONCAT('SELECT ', @column_name); PREPARE stmt FROM @q; END;
