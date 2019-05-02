# Snowflake


This provides a Snowflake driver for migrations.  
It is used whenever the URL of the database starts with `snowflake://`.

For example:

```
snowflake://<user>:<password>@<account>/<database>?schema=<schema>&warehouse=<warehouse>&role=<role>
```


## Introduction

Snowflake database support was added to Migrate, but the Golang Snowflake driver does not support
multiple SQL statements in one round-trip execution.  

Extra functionality was added to split the contents of SQL files into multiple statements at run-time,
but this means that SQL has to be parsed and some limitations on the allowed formatting have been 
introduced...

Parsing works as follows:

* SQL files are split using regular expression delimiter `;\r{0,1}\n` 
* Lines that are pure comments are ignored. That is, those that match regular 
expression `^--.*;\r{0,1}\n`
* Lines that still end with `;` will cause a failure if they contain SQL that is part of 
a multi-line statement.  For examples, see [Known Issues](#Known Issues): 


## Known Issues

#### 1. SQL Formatting & Parsing 

In the Snowflake support that was added, any lines that contain a comment and are also terminated with 
a semicolon `;` will cause a failure 
if the comment characters `--` are not at the beginning of the line.  Sorry that's a bit of a mouthful :-)

The following are examples of syntax that will cause failures:

###### A
 
```
'select * -- get all fields;
 from table;'
```

###### B

```  
' -- get all fields;
  select * from table;'
``` 

* **A** is a failure because a the single statement is split in two because of `fields;`
* **B** is a failure because the comment line is NOT ignored since it does not 
match the regular expression above. A leading space character starts the statement


#### 2. Concurrent Migrations

The added Snowflake driver support does not serialise multiple migrations so 
concurrent deployments are not supported.  

The Migrate golang code provides an interface with a Lock() method that could be
implemented.  

Migrations run by developers are likely to be run against dedicated database clones so this 
is probably not an issue for the majority of cases.  
 